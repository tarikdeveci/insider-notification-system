package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

// ErrLeaseLost means zero rows matched a token fenced write: the caller no
// longer owns the row, so it must not send and must write nothing.
var ErrLeaseLost = errors.New("lease lost")

// Reaper failure reasons. They must equal worker.ReasonExhausted and
// worker.ReasonExpired (store cannot import worker).
const (
	reapReasonExhausted = "exhausted"
	reapReasonExpired   = "expired"

	reapErrExpired   = "expired: max delivery age reached"
	reapErrExhausted = "exhausted: lease expired with no attempts left"
)

// QueueRow is a claimed notification as the worker sees it.
type QueueRow struct {
	ID            string
	BatchID       *string
	Channel       domain.Channel
	Recipient     string
	Content       string
	Priority      domain.Priority
	Attempts      int
	MaxAttempts   int
	NextAttemptAt time.Time
	CreatedAt     time.Time
	ScheduledAt   *time.Time
	CorrelationID string
	LeaseToken    string
	TraceParent   string // W3C traceparent stored by the API; empty when none
}

// ClaimSQL is the per priority claim probe: $1 channel, $2 priority, $3 limit, $4 lease token.
// Exported so the R8 plan test can EXPLAIN the exact statement the worker runs.
const ClaimSQL = `
UPDATE notifications
SET status = 'processing', lease_token = $4::uuid,
    locked_until = now() + interval '30 seconds', updated_at = now()
WHERE id IN (
  SELECT id FROM notifications
  WHERE status = 'pending' AND channel = $1 AND priority = $2
    AND next_attempt_at <= now()
  ORDER BY next_attempt_at, id
  LIMIT $3
  FOR UPDATE SKIP LOCKED)
RETURNING id::text, batch_id::text, channel, recipient, content, priority, attempts, max_attempts,
          next_attempt_at, created_at, scheduled_at, correlation_id, lease_token::text, COALESCE(trace_parent, '')`

// Claim probes priority 0, then 1, then 2 (each probe is the spec SQL with
// $2 = that priority and $3 = rows still needed), stops once n rows are
// claimed, and returns rows sorted by (priority, next_attempt_at, id).
// n < 1 returns nil, nil. token is a uuid string, fresh per Claim call.
func (s *Store) Claim(ctx context.Context, ch domain.Channel, n int, token string) ([]QueueRow, error) {
	if n < 1 {
		return nil, nil
	}
	var out []QueueRow
	for prio := domain.PriorityHigh; prio <= domain.PriorityLow && len(out) < n; prio++ {
		rows, err := s.pool.Query(ctx, ClaimSQL, string(ch), int16(prio), n-len(out), token)
		if err != nil {
			return nil, fmt.Errorf("claim: %w", err)
		}
		for rows.Next() {
			var (
				r        QueueRow
				channel  string
				priority int16
			)
			if err := rows.Scan(&r.ID, &r.BatchID, &channel, &r.Recipient, &r.Content, &priority, &r.Attempts,
				&r.MaxAttempts, &r.NextAttemptAt, &r.CreatedAt, &r.ScheduledAt, &r.CorrelationID, &r.LeaseToken, &r.TraceParent); err != nil {
				rows.Close()
				return nil, fmt.Errorf("claim scan: %w", err)
			}
			r.Channel = domain.Channel(channel)
			r.Priority = domain.Priority(priority)
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("claim rows: %w", err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if !a.NextAttemptAt.Equal(b.NextAttemptAt) {
			return a.NextAttemptAt.Before(b.NextAttemptAt)
		}
		return a.ID < b.ID
	})
	return out, nil
}

// StartAttempt increments attempts and renews the lease in one token fenced
// statement. Zero rows: ErrLeaseLost.
func (s *Store) StartAttempt(ctx context.Context, id, token string) (attempt int, startedAt time.Time, err error) {
	err = s.pool.QueryRow(ctx,
		`UPDATE notifications
		 SET attempts = attempts + 1, locked_until = now() + interval '30 seconds', updated_at = now()
		 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'
		 RETURNING attempts, now()`, id, token).Scan(&attempt, &startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, ErrLeaseLost
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("start attempt: %w", err)
	}
	return attempt, startedAt, nil
}

// FinishAction selects the status transition applied by FinishAttempt.
type FinishAction int

const (
	FinishSent  FinishAction = iota + 1 // status sent, sent_at now(), provider_message_id (NULL when ""), last_error NULL
	FinishRetry                         // status pending, next_attempt_at = NextAttemptAt, last_error = LastError, attempts - 1 when GiveBackAttempt
	FinishFail                          // status failed, last_error = LastError
)

// FinishParams describes the result of one attempt.
type FinishParams struct {
	ID, Token         string
	Action            FinishAction
	AttemptNo         int
	StartedAt         time.Time
	HTTPStatus        int    // 0 stores NULL
	AttemptError      string // "" stores NULL; the classified LastError text, never raw errors
	ProviderMessageID string
	NextAttemptAt     time.Time
	GiveBackAttempt   bool
	LastError         string
}

const finishTail = `, lease_token = NULL, locked_until = NULL, updated_at = now()
 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'`

// FinishAttempt applies the token fenced result UPDATE and inserts the
// notification_attempts row in one transaction. If the UPDATE matches zero
// rows it rolls back, inserts nothing and returns ErrLeaseLost.
func (s *Store) FinishAttempt(ctx context.Context, p FinishParams) error {
	var (
		sql  string
		args = []any{p.ID, p.Token}
	)
	switch p.Action {
	case FinishSent:
		sql = `UPDATE notifications SET status = 'sent', sent_at = now(), provider_message_id = NULLIF($3, ''), last_error = NULL` + finishTail
		args = append(args, p.ProviderMessageID)
	case FinishRetry:
		sql = `UPDATE notifications SET status = 'pending', next_attempt_at = $3, last_error = $4,
		 attempts = attempts - CASE WHEN $5::boolean THEN 1 ELSE 0 END` + finishTail
		args = append(args, p.NextAttemptAt, p.LastError, p.GiveBackAttempt)
	case FinishFail:
		sql = `UPDATE notifications SET status = 'failed', last_error = $3` + finishTail
		args = append(args, p.LastError)
	default:
		return fmt.Errorf("finish attempt: unknown action %d", p.Action)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("finish attempt begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("finish attempt update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO notification_attempts (notification_id, attempt_no, started_at, finished_at, http_status, error, duration_ms)
		 VALUES ($1::uuid, $2, $3::timestamptz, now(), NULLIF($4::int, 0), NULLIF($5::text, ''),
		         GREATEST(0, (extract(epoch FROM (now() - $3::timestamptz)) * 1000)::int))`,
		p.ID, p.AttemptNo, p.StartedAt, p.HTTPStatus, p.AttemptError)
	if err != nil {
		return fmt.Errorf("finish attempt insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finish attempt commit: %w", err)
	}
	return nil
}

// Release returns a claimed but unstarted row to pending: lease cleared,
// attempts and next_attempt_at unchanged. Zero rows: ErrLeaseLost.
func (s *Store) Release(ctx context.Context, id, token string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE notifications SET status = 'pending', lease_token = NULL, locked_until = NULL, updated_at = now()
		 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'`, id, token)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// ReapedRow is one row moved out of processing by Reap.
type ReapedRow struct {
	ID      string
	Channel domain.Channel
	Status  domain.Status // pending or failed
	Reason  string        // "" for requeued, else "exhausted" or "expired"
}

// Reap handles processing rows whose lease expired, in one UPDATE (no token:
// the predicate itself proves expiry). Rows past max delivery age fail with
// reason expired, rows with no attempts left fail with reason exhausted, the
// rest return to pending with next_attempt_at = now(). No attempt row is written.
func (s *Store) Reap(ctx context.Context, maxAge time.Duration) ([]ReapedRow, error) {
	const expired = `now() >= COALESCE(scheduled_at, created_at) + make_interval(secs => $1::float8)`
	rows, err := s.pool.Query(ctx,
		`UPDATE notifications SET
		   status = CASE WHEN `+expired+` OR attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
		   last_error = CASE WHEN `+expired+` THEN $2::text
		                     WHEN attempts >= max_attempts THEN $3::text ELSE last_error END,
		   next_attempt_at = CASE WHEN `+expired+` OR attempts >= max_attempts THEN next_attempt_at ELSE now() END,
		   lease_token = NULL, locked_until = NULL, updated_at = now()
		 WHERE status = 'processing' AND locked_until < now()
		 RETURNING id::text, channel, status, last_error`,
		maxAge.Seconds(), reapErrExpired, reapErrExhausted)
	if err != nil {
		return nil, fmt.Errorf("reap: %w", err)
	}
	defer rows.Close()
	var out []ReapedRow
	for rows.Next() {
		var (
			r       ReapedRow
			channel string
			status  string
			lastErr *string
		)
		if err := rows.Scan(&r.ID, &channel, &status, &lastErr); err != nil {
			return nil, fmt.Errorf("reap scan: %w", err)
		}
		r.Channel = domain.Channel(channel)
		r.Status = domain.Status(status)
		if r.Status == domain.StatusFailed && lastErr != nil {
			switch *lastErr {
			case reapErrExpired:
				r.Reason = reapReasonExpired
			case reapErrExhausted:
				r.Reason = reapReasonExhausted
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reap rows: %w", err)
	}
	return out, nil
}

// DepthRow is one queue depth sample.
type DepthRow struct {
	Channel  domain.Channel
	Priority domain.Priority
	State    string // "ready" (pending, next_attempt_at <= now()), "scheduled" (pending, later), "processing"
	Count    int
}

// QueueDepth returns, per (channel, priority) group over pending and
// processing rows, one DepthRow per state (zero counts allowed).
func (s *Store) QueueDepth(ctx context.Context) ([]DepthRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT channel, priority,
		        count(*) FILTER (WHERE status = 'pending' AND next_attempt_at <= now()),
		        count(*) FILTER (WHERE status = 'pending' AND next_attempt_at > now()),
		        count(*) FILTER (WHERE status = 'processing')
		 FROM notifications WHERE status IN ('pending', 'processing')
		 GROUP BY channel, priority ORDER BY channel, priority`)
	if err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	defer rows.Close()
	var out []DepthRow
	for rows.Next() {
		var (
			ch                       string
			prio                     int16
			ready, scheduled, inProc int
		)
		if err := rows.Scan(&ch, &prio, &ready, &scheduled, &inProc); err != nil {
			return nil, fmt.Errorf("queue depth scan: %w", err)
		}
		c, p := domain.Channel(ch), domain.Priority(prio)
		out = append(out,
			DepthRow{c, p, "ready", ready},
			DepthRow{c, p, "scheduled", scheduled},
			DepthRow{c, p, "processing", inProc})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue depth rows: %w", err)
	}
	return out, nil
}
