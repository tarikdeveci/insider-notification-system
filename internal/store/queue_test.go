package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tarikdeveci/insider-notification-system/internal/config"
	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

func newToken(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var tok string
	if err := pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// claimOne creates one sms row and claims it.
func claimOne(t *testing.T, s *store.Store, pool *pgxpool.Pool) (store.QueueRow, string) {
	t.Helper()
	mustCreate(t, s, domain.ChannelSMS, "q")
	tok := newToken(t, pool)
	rows, err := s.Claim(context.Background(), domain.ChannelSMS, 1, tok)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v %d", err, len(rows))
	}
	return rows[0], tok
}

type rowState struct {
	status   string
	attempts int
	lease    *string
	locked   *time.Time
	lastErr  *string
	msgID    *string
	sentAt   *time.Time
	next     time.Time
}

func state(t *testing.T, pool *pgxpool.Pool, id string) rowState {
	t.Helper()
	var r rowState
	err := pool.QueryRow(context.Background(),
		`SELECT status, attempts, lease_token::text, locked_until, last_error, provider_message_id, sent_at, next_attempt_at
		 FROM notifications WHERE id = $1::uuid`, id).Scan(&r.status, &r.attempts, &r.lease, &r.locked, &r.lastErr, &r.msgID, &r.sentAt, &r.next)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func attemptCount(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_attempts WHERE notification_id = $1::uuid`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// config validates RATELIMIT_MAX_WAIT against config.LeaseDuration; the SQL
// literal must stay equal to it.
func TestClaimLeaseMatchesConfigLeaseDuration(t *testing.T) {
	s, pool := newStore(t)
	row, _ := claimOne(t, s, pool)
	var secs float64
	err := pool.QueryRow(context.Background(),
		`SELECT extract(epoch FROM (locked_until - now())) FROM notifications WHERE id = $1::uuid`, row.ID).Scan(&secs)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(secs * float64(time.Second)); got < config.LeaseDuration-2*time.Second || got > config.LeaseDuration {
		t.Fatalf("claim lease is %s, config.LeaseDuration is %s", got, config.LeaseDuration)
	}
}

// A later priority probe that fails must not drop rows an earlier probe already
// moved to processing: Claim returns them together with the error.
func TestClaimReturnsRowsOfEarlierProbeWithError(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	high, err := s.CreateNotification(ctx, store.CreateParams{
		New:           domain.NewNotification{Channel: domain.ChannelSMS, Recipient: recipientFor(domain.ChannelSMS), Content: "h", Priority: domain.PriorityHigh},
		CorrelationID: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, domain.ChannelSMS, "normal") // priority 1: its probe is made to fail
	exec(t, pool, `CREATE FUNCTION fail_normal() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'probe failure'; END $$ LANGUAGE plpgsql`)
	exec(t, pool, `CREATE TRIGGER fail_normal BEFORE UPDATE ON notifications FOR EACH ROW WHEN (NEW.priority = 1 AND NEW.status = 'processing') EXECUTE FUNCTION fail_normal()`)

	rows, err := s.Claim(ctx, domain.ChannelSMS, 5, newToken(t, pool))
	if err == nil {
		t.Fatal("want the failed probe's error")
	}
	if len(rows) != 1 || rows[0].ID != high.Notification.ID {
		t.Fatalf("want the high priority row returned with the error, got %d rows", len(rows))
	}
	if st := state(t, pool, high.Notification.ID); st.status != "processing" {
		t.Fatalf("high priority row status = %s, want processing", st.status)
	}
}

func TestClaimNeverReturnsRowTwice(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	const total, workers = 60, 8
	for i := 0; i < total; i++ {
		mustCreate(t, s, domain.ChannelSMS, "c")
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		tok := newToken(t, pool)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rows, err := s.Claim(ctx, domain.ChannelSMS, 5, tok)
				if err != nil {
					errs <- err
					return
				}
				if len(rows) == 0 {
					return
				}
				mu.Lock()
				for _, r := range rows {
					seen[r.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(seen) != total {
		t.Fatalf("claimed %d distinct rows, want %d", len(seen), total)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("row %s claimed %d times", id, c)
		}
	}
}

func TestClaimOneProbePerPriorityOrder(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	mk := func(prio int, offsetSec int) string {
		n := mustCreate(t, s, domain.ChannelSMS, "p")
		exec(t, pool, `UPDATE notifications SET priority = $2, next_attempt_at = now() - make_interval(secs => $3) WHERE id = $1::uuid`,
			n.ID, prio, float64(offsetSec))
		return n.ID
	}
	low := mk(2, 100)
	normalNew := mk(1, 10)
	normalOld := mk(1, 50)
	high := mk(0, 5)
	rows, err := s.Claim(ctx, domain.ChannelSMS, 10, newToken(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{high, normalOld, normalNew, low}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows", len(rows))
	}
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("pos %d: got %s want %s", i, rows[i].ID, id)
		}
	}
	// n smaller than the backlog stops after the high probe and takes the best rows only.
	exec(t, pool, `UPDATE notifications SET status = 'pending', lease_token = NULL, locked_until = NULL`)
	rows, err = s.Claim(ctx, domain.ChannelSMS, 2, newToken(t, pool))
	if err != nil || len(rows) != 2 || rows[0].ID != high || rows[1].ID != normalOld {
		t.Fatalf("limited claim: %v %+v", err, rows)
	}
	if st := state(t, pool, low); st.status != "pending" {
		t.Fatalf("low must stay pending, got %s", st.status)
	}
}

func TestClaimNonPositiveN(t *testing.T) {
	s, pool := newStore(t)
	mustCreate(t, s, domain.ChannelSMS, "z")
	rows, err := s.Claim(context.Background(), domain.ChannelSMS, 0, newToken(t, pool))
	if err != nil || rows != nil {
		t.Fatalf("got %v %v", rows, err)
	}
}

func TestClaimSkipsFutureAndNonPending(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	future := mustCreate(t, s, domain.ChannelSMS, "f")
	exec(t, pool, `UPDATE notifications SET next_attempt_at = now() + interval '1 hour' WHERE id = $1::uuid`, future.ID)
	sent := mustCreate(t, s, domain.ChannelSMS, "s")
	exec(t, pool, `UPDATE notifications SET status = 'sent' WHERE id = $1::uuid`, sent.ID)
	other := mustCreate(t, s, domain.ChannelEmail, "e")
	ready := mustCreate(t, s, domain.ChannelSMS, "r")
	rows, err := s.Claim(ctx, domain.ChannelSMS, 10, newToken(t, pool))
	if err != nil || len(rows) != 1 || rows[0].ID != ready.ID {
		t.Fatalf("claim: %v %+v", err, rows)
	}
	if st := state(t, pool, other.ID); st.status != "pending" {
		t.Fatalf("other channel row touched: %s", st.status)
	}
}

func TestClaimSetsLeaseTokenAndLockedUntil(t *testing.T) {
	s, pool := newStore(t)
	row, tok := claimOne(t, s, pool)
	if row.LeaseToken != tok {
		t.Fatalf("returned token %s want %s", row.LeaseToken, tok)
	}
	if row.Channel != domain.ChannelSMS || row.Recipient == "" || row.Content != "q" || row.MaxAttempts != 8 || row.CorrelationID != "test" {
		t.Fatalf("row fields: %+v", row)
	}
	var okLease bool
	err := pool.QueryRow(context.Background(),
		`SELECT status = 'processing' AND lease_token = $2::uuid
		        AND locked_until > now() + interval '25 seconds' AND locked_until <= now() + interval '30 seconds'
		 FROM notifications WHERE id = $1::uuid`, row.ID, tok).Scan(&okLease)
	if err != nil || !okLease {
		t.Fatalf("lease not set: %v %v", okLease, err)
	}
}

func TestStartAttemptRequiresLeaseToken(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	row, tok := claimOne(t, s, pool)
	if _, _, err := s.StartAttempt(ctx, row.ID, newToken(t, pool)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale token: %v", err)
	}
	if st := state(t, pool, row.ID); st.attempts != 0 {
		t.Fatalf("attempts changed to %d", st.attempts)
	}
	attempt, started, err := s.StartAttempt(ctx, row.ID, tok)
	if err != nil || attempt != 1 || started.IsZero() {
		t.Fatalf("start: %d %v %v", attempt, started, err)
	}
	attempt, _, err = s.StartAttempt(ctx, row.ID, tok)
	if err != nil || attempt != 2 {
		t.Fatalf("second start: %d %v", attempt, err)
	}
}

func TestClaimUsesFreshTokenFencing(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustCreate(t, s, domain.ChannelSMS, "a")
	tokA := newToken(t, pool)
	rowsA, err := s.Claim(ctx, domain.ChannelSMS, 1, tokA)
	if err != nil || len(rowsA) != 1 {
		t.Fatal(err)
	}
	b := mustCreate(t, s, domain.ChannelSMS, "b")
	tokB := newToken(t, pool)
	rowsB, err := s.Claim(ctx, domain.ChannelSMS, 1, tokB)
	if err != nil || len(rowsB) != 1 || rowsB[0].ID != b.ID || rowsA[0].ID != a.ID {
		t.Fatalf("claims: %v %+v", err, rowsB)
	}
	if _, _, err := s.StartAttempt(ctx, rowsA[0].ID, tokB); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("A row with B token: %v", err)
	}
	if _, _, err := s.StartAttempt(ctx, rowsB[0].ID, tokA); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("B row with A token: %v", err)
	}
	if _, _, err := s.StartAttempt(ctx, rowsA[0].ID, tokA); err != nil {
		t.Fatal(err)
	}
}

func expireLease(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	exec(t, pool, `UPDATE notifications SET locked_until = now() - interval '1 second' WHERE id = $1::uuid`, id)
}

func TestStaleWorkerCannotWriteResult(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	row, oldTok := claimOne(t, s, pool)
	attempt, started, err := s.StartAttempt(ctx, row.ID, oldTok)
	if err != nil {
		t.Fatal(err)
	}
	expireLease(t, pool, row.ID)
	reaped, err := s.Reap(ctx, 24*time.Hour)
	if err != nil || len(reaped) != 1 || reaped[0].Status != domain.StatusPending {
		t.Fatalf("reap: %v %+v", err, reaped)
	}
	newTok := newToken(t, pool)
	rows, err := s.Claim(ctx, domain.ChannelSMS, 1, newTok)
	if err != nil || len(rows) != 1 || rows[0].ID != row.ID {
		t.Fatalf("reclaim: %v %+v", err, rows)
	}
	err = s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: oldTok, Action: store.FinishSent, AttemptNo: attempt, StartedAt: started, HTTPStatus: 202})
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale finish: %v", err)
	}
	if n := attemptCount(t, pool, row.ID); n != 1 { // the reaper's lease_expired row, nothing from the stale worker
		t.Fatalf("attempt rows = %d, want only the reaper's lease_expired row", n)
	}
	if st := state(t, pool, row.ID); st.status != "processing" || st.lease == nil || *st.lease != newTok {
		t.Fatalf("row disturbed: %+v", st)
	}
}

func TestResultTransactionWritesAttemptRow(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	t.Run("sent", func(t *testing.T) {
		row, tok := claimOne(t, s, pool)
		attempt, started, err := s.StartAttempt(ctx, row.ID, tok)
		if err != nil {
			t.Fatal(err)
		}
		err = s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: store.FinishSent, AttemptNo: attempt,
			StartedAt: started, HTTPStatus: 202, ProviderMessageID: "m-1"})
		if err != nil {
			t.Fatal(err)
		}
		st := state(t, pool, row.ID)
		if st.status != "sent" || st.sentAt == nil || st.msgID == nil || *st.msgID != "m-1" || st.lease != nil || st.locked != nil || st.lastErr != nil {
			t.Fatalf("state: %+v", st)
		}
		var (
			no     int
			http   *int
			errTxt *string
			fin    *time.Time
			dur    *int
		)
		err = pool.QueryRow(ctx, `SELECT attempt_no, http_status, error, finished_at, duration_ms FROM notification_attempts WHERE notification_id = $1::uuid`, row.ID).
			Scan(&no, &http, &errTxt, &fin, &dur)
		if err != nil || no != 1 || http == nil || *http != 202 || errTxt != nil || fin == nil || dur == nil || *dur < 0 {
			t.Fatalf("attempt row: %v %d %v %v %v %v", err, no, http, errTxt, fin, dur)
		}
	})

	t.Run("sent without message id stores NULL", func(t *testing.T) {
		row, tok := claimOne(t, s, pool)
		attempt, started, _ := s.StartAttempt(ctx, row.ID, tok)
		if err := s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: store.FinishSent, AttemptNo: attempt, StartedAt: started, HTTPStatus: 202}); err != nil {
			t.Fatal(err)
		}
		if st := state(t, pool, row.ID); st.msgID != nil {
			t.Fatalf("message id %v", *st.msgID)
		}
	})

	t.Run("retry", func(t *testing.T) {
		row, tok := claimOne(t, s, pool)
		attempt, started, _ := s.StartAttempt(ctx, row.ID, tok)
		before := dbNow(t, pool)
		err := s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: store.FinishRetry, AttemptNo: attempt, StartedAt: started,
			HTTPStatus: 503, AttemptError: "provider 5xx", RetryDelay: time.Hour, LastError: "provider 5xx"})
		if err != nil {
			t.Fatal(err)
		}
		after := dbNow(t, pool)
		st := state(t, pool, row.ID)
		if st.status != "pending" || st.attempts != 1 || st.lastErr == nil || *st.lastErr != "provider 5xx" || st.lease != nil || st.locked != nil {
			t.Fatalf("state: %+v", st)
		}
		// The retry is scheduled on the database clock, not the worker's.
		if st.next.Before(before.Add(time.Hour)) || st.next.After(after.Add(time.Hour)) {
			t.Fatalf("next_attempt_at %v not within db now + 1h [%v, %v]", st.next, before.Add(time.Hour), after.Add(time.Hour))
		}
		var e *string
		if err := pool.QueryRow(ctx, `SELECT error FROM notification_attempts WHERE notification_id = $1::uuid`, row.ID).Scan(&e); err != nil || e == nil || *e != "provider 5xx" {
			t.Fatalf("attempt error: %v %v", err, e)
		}
	})

	t.Run("fail", func(t *testing.T) {
		row, tok := claimOne(t, s, pool)
		attempt, started, _ := s.StartAttempt(ctx, row.ID, tok)
		err := s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: store.FinishFail, AttemptNo: attempt, StartedAt: started,
			HTTPStatus: 400, AttemptError: "permanent", LastError: "permanent: provider rejected"})
		if err != nil {
			t.Fatal(err)
		}
		st := state(t, pool, row.ID)
		if st.status != "failed" || st.lastErr == nil || *st.lastErr != "permanent: provider rejected" || st.lease != nil {
			t.Fatalf("state: %+v", st)
		}
		if n := attemptCount(t, pool, row.ID); n != 1 {
			t.Fatalf("attempt rows %d", n)
		}
	})

	t.Run("unknown action writes nothing", func(t *testing.T) {
		row, tok := claimOne(t, s, pool)
		if err := s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: 99}); err == nil || errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("want plain error, got %v", err)
		}
		if st := state(t, pool, row.ID); st.status != "processing" {
			t.Fatalf("status %s", st.status)
		}
	})

	t.Run("failed insert rolls back the update", func(t *testing.T) {
		row, tok := claimOne(t, s, pool)
		_, started, _ := s.StartAttempt(ctx, row.ID, tok)
		exec(t, pool, `ALTER TABLE notification_attempts ADD CONSTRAINT no_insert CHECK (false) NOT VALID`)
		defer exec(t, pool, `ALTER TABLE notification_attempts DROP CONSTRAINT no_insert`)
		err := s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: store.FinishSent, AttemptNo: 1, StartedAt: started})
		if err == nil || errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("want insert error, got %v", err)
		}
		if st := state(t, pool, row.ID); st.status != "processing" || st.sentAt != nil {
			t.Fatalf("update not rolled back: %+v", st)
		}
	})
}

func TestThrottledResultGivesAttemptBack(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	row, tok := claimOne(t, s, pool)
	attempt, started, err := s.StartAttempt(ctx, row.ID, tok)
	if err != nil || attempt != 1 {
		t.Fatal(err)
	}
	err = s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: store.FinishRetry, AttemptNo: attempt, StartedAt: started,
		HTTPStatus: 429, AttemptError: "throttled", RetryDelay: time.Second, GiveBackAttempt: true, LastError: "throttled"})
	if err != nil {
		t.Fatal(err)
	}
	st := state(t, pool, row.ID)
	if st.status != "pending" || st.attempts != 0 {
		t.Fatalf("state: %+v", st)
	}
	if n := attemptCount(t, pool, row.ID); n != 1 {
		t.Fatalf("the attempt row is still recorded, got %d", n)
	}
}

// A release with a delay pushes next_attempt_at into the future by the database
// clock, so the same rows do not come straight back from the claim.
func TestReleaseWithDelayDefersNextAttempt(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	row, tok := claimOne(t, s, pool)
	if err := s.Release(ctx, row.ID, tok, time.Hour); err != nil {
		t.Fatal(err)
	}
	var secs float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM (next_attempt_at - now())) FROM notifications WHERE id = $1::uuid`, row.ID).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs < 3500 || secs > 3601 {
		t.Fatalf("next_attempt_at is %.0fs ahead, want about 3600", secs)
	}
	if rows, err := s.Claim(ctx, domain.ChannelSMS, 1, newToken(t, pool)); err != nil || len(rows) != 0 {
		t.Fatalf("a delayed row must not be claimable: %v %d", err, len(rows))
	}
}

type historyRow struct {
	seq, attemptNo int
	errText        string
}

func history(t *testing.T, pool *pgxpool.Pool, id string) []historyRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT seq, attempt_no, COALESCE(error, '') FROM notification_attempts WHERE notification_id = $1::uuid ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []historyRow
	for rows.Next() {
		var h historyRow
		if err := rows.Scan(&h.seq, &h.attemptNo, &h.errText); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

// attempt_no is the notifications.attempts counter at that attempt, so a 429
// (which refunds the attempt) leaves rows that share it. seq is the unambiguous
// per notification order, and the reaper records a lost lease as its own row.
func TestAttemptHistoryHasUnambiguousSequence(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	row, tok := claimOne(t, s, pool)
	for i := 0; i < 2; i++ { // two throttled attempts share attempt_no 1
		attempt, started, err := s.StartAttempt(ctx, row.ID, tok)
		if err != nil {
			t.Fatal(err)
		}
		err = s.FinishAttempt(ctx, store.FinishParams{ID: row.ID, Token: tok, Action: store.FinishRetry, AttemptNo: attempt, StartedAt: started,
			HTTPStatus: 429, AttemptError: "throttled", RetryDelay: 0, GiveBackAttempt: true, LastError: "throttled"})
		if err != nil {
			t.Fatal(err)
		}
		tok = newToken(t, pool)
		if rows, err := s.Claim(ctx, domain.ChannelSMS, 1, tok); err != nil || len(rows) != 1 {
			t.Fatalf("reclaim: %v %d", err, len(rows))
		}
	}
	if _, _, err := s.StartAttempt(ctx, row.ID, tok); err != nil { // attempt 1 again, then its lease expires
		t.Fatal(err)
	}
	expireLease(t, pool, row.ID)
	if _, err := s.Reap(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got := history(t, pool, row.ID)
	want := []historyRow{{1, 1, "throttled"}, {2, 1, "throttled"}, {3, 1, "lease_expired"}}
	if len(got) != len(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history = %+v, want %+v", got, want)
		}
	}
	attempts, err := s.ListAttempts(ctx, row.ID)
	if err != nil || len(attempts) != 3 || attempts[2].Error == nil || *attempts[2].Error != "lease_expired" {
		t.Fatalf("ListAttempts: %v %+v", err, attempts)
	}
}

func TestReleaseDoesNotUseAttempt(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	row, tok := claimOne(t, s, pool)
	if err := s.Release(ctx, row.ID, newToken(t, pool), 0); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale release: %v", err)
	}
	if err := s.Release(ctx, row.ID, tok, 0); err != nil {
		t.Fatal(err)
	}
	st := state(t, pool, row.ID)
	if st.status != "pending" || st.attempts != 0 || st.lease != nil || st.locked != nil || !st.next.Equal(row.NextAttemptAt) {
		t.Fatalf("state: %+v", st)
	}
	if err := s.Release(ctx, row.ID, tok, 0); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("second release: %v", err)
	}
	if n := attemptCount(t, pool, row.ID); n != 0 {
		t.Fatalf("attempt rows %d", n)
	}
}

func TestReaperRequeuesExpiredLeases(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	expired, tok := claimOne(t, s, pool)
	if _, _, err := s.StartAttempt(ctx, expired.ID, tok); err != nil {
		t.Fatal(err)
	}
	expireLease(t, pool, expired.ID)
	live, _ := claimOne(t, s, pool)

	reaped, err := s.Reap(ctx, 24*time.Hour)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("reap: %v %+v", err, reaped)
	}
	r := reaped[0]
	if r.ID != expired.ID || r.Channel != domain.ChannelSMS || r.Status != domain.StatusPending || r.Reason != "" {
		t.Fatalf("reaped: %+v", r)
	}
	st := state(t, pool, expired.ID)
	if st.status != "pending" || st.attempts != 1 || st.lease != nil || st.locked != nil || st.lastErr != nil {
		t.Fatalf("state: %+v", st)
	}
	if n := attemptCount(t, pool, expired.ID); n != 1 {
		t.Fatalf("reaper must write one lease_expired attempt row, got %d", n)
	}
	if n := attemptCount(t, pool, live.ID); n != 0 {
		t.Fatalf("a live lease got %d attempt rows", n)
	}
	if st := state(t, pool, live.ID); st.status != "processing" {
		t.Fatalf("live lease reaped: %s", st.status)
	}
	// The old token must no longer work.
	if _, _, err := s.StartAttempt(ctx, expired.ID, tok); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("old token after reap: %v", err)
	}
}

func TestReaperFailsWhenAttemptsExhaustedOrExpired(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	exhausted, _ := claimOne(t, s, pool)
	exec(t, pool, `UPDATE notifications SET attempts = max_attempts WHERE id = $1::uuid`, exhausted.ID)
	aged, _ := claimOne(t, s, pool)
	exec(t, pool, `UPDATE notifications SET created_at = now() - interval '2 hours' WHERE id = $1::uuid`, aged.ID)
	scheduled, _ := claimOne(t, s, pool)
	exec(t, pool, `UPDATE notifications SET created_at = now() - interval '5 hours', scheduled_at = now() - interval '10 minutes' WHERE id = $1::uuid`, scheduled.ID)
	both, _ := claimOne(t, s, pool)
	exec(t, pool, `UPDATE notifications SET attempts = max_attempts, created_at = now() - interval '2 hours' WHERE id = $1::uuid`, both.ID)
	for _, id := range []string{exhausted.ID, aged.ID, scheduled.ID, both.ID} {
		expireLease(t, pool, id)
	}

	reaped, err := s.Reap(ctx, time.Hour)
	if err != nil || len(reaped) != 4 {
		t.Fatalf("reap: %v %+v", err, reaped)
	}
	got := map[string]store.ReapedRow{}
	for _, r := range reaped {
		got[r.ID] = r
	}
	check := func(name, id, reason, lastErr, status string) {
		t.Helper()
		r := got[id]
		if r.Status != domain.StatusFailed || r.Reason != reason {
			t.Fatalf("%s: reaped %+v", name, r)
		}
		st := state(t, pool, id)
		if st.status != status || st.lastErr == nil || *st.lastErr != lastErr || st.lease != nil || st.locked != nil {
			t.Fatalf("%s: state %+v", name, st)
		}
	}
	check("exhausted", exhausted.ID, "exhausted", "exhausted: lease expired with no attempts left", "failed")
	check("aged", aged.ID, "expired", "expired: max delivery age reached", "failed")
	check("both prefers expired", both.ID, "expired", "expired: max delivery age reached", "failed")
	// scheduled_at 10 minutes ago is inside the one hour age bound, so it is requeued, not failed.
	if r := got[scheduled.ID]; r.Status != domain.StatusPending || r.Reason != "" {
		t.Fatalf("scheduled row measured from created_at: %+v", r)
	}
}

func TestQueueDepth(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	mustCreate(t, s, domain.ChannelSMS, "ready")
	fut := mustCreate(t, s, domain.ChannelSMS, "future")
	exec(t, pool, `UPDATE notifications SET next_attempt_at = now() + interval '1 hour' WHERE id = $1::uuid`, fut.ID)
	mustCreate(t, s, domain.ChannelEmail, "e")
	if rows, err := s.Claim(ctx, domain.ChannelEmail, 1, newToken(t, pool)); err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	done := mustCreate(t, s, domain.ChannelPush, "done")
	exec(t, pool, `UPDATE notifications SET status = 'sent' WHERE id = $1::uuid`, done.ID)

	rows, err := s.QueueDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		ch    domain.Channel
		prio  domain.Priority
		state string
	}
	got := map[key]int{}
	for _, r := range rows {
		got[key{r.Channel, r.Priority, r.State}] = r.Count
	}
	want := map[key]int{
		{domain.ChannelSMS, domain.PriorityNormal, "ready"}:        1,
		{domain.ChannelSMS, domain.PriorityNormal, "scheduled"}:    1,
		{domain.ChannelSMS, domain.PriorityNormal, "processing"}:   0,
		{domain.ChannelEmail, domain.PriorityNormal, "ready"}:      0,
		{domain.ChannelEmail, domain.PriorityNormal, "scheduled"}:  0,
		{domain.ChannelEmail, domain.PriorityNormal, "processing"}: 1,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if c, ok := got[k]; !ok || c != v {
			t.Fatalf("%v: got %d (present=%v) want %d", k, c, ok, v)
		}
	}
}

func TestCancelVsClaimRace(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		n := mustCreate(t, s, domain.ChannelSMS, "race")
		tok := newToken(t, pool)
		var cancelled, claimed atomic.Int32
		var cancelErr, claimErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, cancelErr = s.CancelNotification(ctx, n.ID)
			if cancelErr == nil {
				cancelled.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			var rows []store.QueueRow
			rows, claimErr = s.Claim(ctx, domain.ChannelSMS, 1, tok)
			if claimErr == nil && len(rows) == 1 && rows[0].ID == n.ID {
				claimed.Add(1)
			}
		}()
		wg.Wait()
		if cancelErr != nil && !errors.Is(cancelErr, store.ErrNotPending) {
			t.Fatalf("round %d: cancel: %v", round, cancelErr)
		}
		if claimErr != nil {
			t.Fatalf("round %d: claim: %v", round, claimErr)
		}
		if cancelled.Load()+claimed.Load() != 1 {
			t.Fatalf("round %d: cancelled=%d claimed=%d", round, cancelled.Load(), claimed.Load())
		}
		// Clean up a claimed row so the next round's claim sees only its own row.
		if claimed.Load() == 1 {
			exec(t, pool, `UPDATE notifications SET status = 'cancelled' WHERE id = $1::uuid`, n.ID)
		}
	}
}

func TestMaxDeliveryAgeUsesScheduledAt(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	at := time.Now().Add(-10 * time.Minute)
	nn := sample(domain.ChannelSMS, "sched")
	nn.ScheduledAt = &at
	res, err := s.CreateNotification(ctx, store.CreateParams{New: nn, CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Notification.ID
	rows, err := s.Claim(ctx, domain.ChannelSMS, 1, newToken(t, pool))
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("claim: %v %+v", err, rows)
	}
	exec(t, pool, `UPDATE notifications SET created_at = now() - interval '5 hours' WHERE id = $1::uuid`, id)
	expireLease(t, pool, id)

	reaped, err := s.Reap(ctx, time.Hour)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("reap: %v %+v", err, reaped)
	}
	if r := reaped[0]; r.Status != domain.StatusPending || r.Reason != "" {
		t.Fatalf("scheduled row measured from created_at: %+v", r)
	}
	if st := state(t, pool, id); st.status != "pending" {
		t.Fatalf("status %s", st.status)
	}
}

// A pending row past the max delivery age must not be sent: the claim fails it
// as expired in the same statement and hands it back marked, without a lease.
// The age runs from scheduled_at when set, so a row scheduled for later is not
// expired by its old created_at. Claim (no age) never expires a row.
func TestClaimWithAgeFailsExpiredPendingRows(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	old := mustCreate(t, s, domain.ChannelSMS, "old")
	fresh := mustCreate(t, s, domain.ChannelSMS, "fresh")
	sched := mustCreate(t, s, domain.ChannelSMS, "scheduled")
	if _, err := pool.Exec(ctx, `UPDATE notifications SET created_at = now() - interval '25 hours' WHERE id = ANY($1::uuid[])`,
		[]string{old.ID, sched.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET scheduled_at = now() - interval '1 hour' WHERE id = $1::uuid`, sched.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Claim(ctx, domain.ChannelEmail, 10, newToken(t, pool)); err != nil || len(rows) != 0 {
		t.Fatalf("other channel claim: %v %d", err, len(rows))
	}

	rows, err := s.ClaimWithAge(ctx, domain.ChannelSMS, 10, newToken(t, pool), 24*time.Hour)
	if err != nil || len(rows) != 3 {
		t.Fatalf("claim: %v %d", err, len(rows))
	}
	byID := map[string]store.QueueRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r := byID[old.ID]; !r.Expired || r.LeaseToken != "" {
		t.Fatalf("old row: expired=%v lease=%q, want expired with no lease", r.Expired, r.LeaseToken)
	}
	for _, id := range []string{fresh.ID, sched.ID} {
		if r := byID[id]; r.Expired || r.LeaseToken == "" {
			t.Fatalf("row %s: expired=%v lease=%q, want a live claim", id, r.Expired, r.LeaseToken)
		}
		if st := state(t, pool, id); st.status != "processing" {
			t.Fatalf("row %s status %s, want processing", id, st.status)
		}
	}
	st := state(t, pool, old.ID)
	if st.status != "failed" || st.lease != nil || st.locked != nil || st.attempts != 0 ||
		st.lastErr == nil || *st.lastErr != "expired: max delivery age reached" {
		t.Fatalf("old row state: %+v", st)
	}
}

func TestClaimWithoutAgeNeverExpires(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	old := mustCreate(t, s, domain.ChannelSMS, "old")
	if _, err := pool.Exec(ctx, `UPDATE notifications SET created_at = now() - interval '400 days' WHERE id = $1::uuid`, old.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Claim(ctx, domain.ChannelSMS, 1, newToken(t, pool))
	if err != nil || len(rows) != 1 || rows[0].Expired {
		t.Fatalf("claim: %v %+v", err, rows)
	}
}
