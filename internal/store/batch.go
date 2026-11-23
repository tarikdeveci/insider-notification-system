package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

// CreateBatchParams carries the items of one batch plus its idempotency data.
type CreateBatchParams struct {
	Items          []domain.NewNotification
	IdempotencyKey string // empty means none
	RequestHash    string
	CorrelationID  string
}

// BatchResult is the outcome of CreateBatch.
type BatchResult struct {
	BatchID  string
	IDs      []string // notification ids in request order (by batch_position)
	Replayed bool
}

// BatchSummary is a batch with its per-status notification counts.
type BatchSummary struct {
	ID        string
	Total     int
	CreatedAt time.Time
	Counts    map[domain.Status]int // every status present, zero filled
}

// CreateBatch inserts a batch and all its notifications in one transaction.
// When the idempotency key was already used, it returns the stored batch with
// Replayed set, or ErrIdempotencyConflict if the request hash differs.
func (s *Store) CreateBatch(ctx context.Context, p CreateBatchParams) (BatchResult, error) {
	var key any // nil never conflicts on the unique index
	if p.IdempotencyKey != "" {
		key = p.IdempotencyKey
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BatchResult{}, fmt.Errorf("begin batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var batchID string
	err = tx.QueryRow(ctx,
		`INSERT INTO batches (idempotency_key, request_hash, total) VALUES ($1, $2, $3)
		 ON CONFLICT (idempotency_key) DO NOTHING RETURNING id::text`,
		key, p.RequestHash, len(p.Items)).Scan(&batchID)
	if errors.Is(err, pgx.ErrNoRows) && p.IdempotencyKey != "" {
		_ = tx.Rollback(ctx)
		return s.replayBatch(ctx, p.IdempotencyKey, p.RequestHash)
	}
	if err != nil {
		return BatchResult{}, fmt.Errorf("insert batch: %w", err)
	}

	n := len(p.Items)
	pos := make([]int32, n)
	channels := make([]string, n)
	recipients := make([]string, n)
	contents := make([]string, n)
	priorities := make([]int16, n)
	for i, it := range p.Items {
		pos[i] = int32(i)
		channels[i] = string(it.Channel)
		recipients[i] = it.Recipient
		contents[i] = it.Content
		priorities[i] = int16(it.Priority)
	}
	rows, err := tx.Query(ctx,
		`INSERT INTO notifications (batch_id, batch_position, channel, recipient, content, priority, correlation_id)
		 SELECT $1::uuid, t.pos, t.channel, t.recipient, t.content, t.priority, $7
		 FROM unnest($2::int[], $3::text[], $4::text[], $5::text[], $6::smallint[]) AS t(pos, channel, recipient, content, priority)
		 RETURNING batch_position, id::text`,
		batchID, pos, channels, recipients, contents, priorities, p.CorrelationID)
	if err != nil {
		return BatchResult{}, fmt.Errorf("insert batch items: %w", err)
	}
	ids := make([]string, n)
	for rows.Next() {
		var at int
		var id string
		if err := rows.Scan(&at, &id); err != nil {
			rows.Close()
			return BatchResult{}, fmt.Errorf("scan batch item: %w", err)
		}
		if at < 0 || at >= n {
			rows.Close()
			return BatchResult{}, fmt.Errorf("batch position %d out of range", at)
		}
		ids[at] = id
	}
	if err := rows.Err(); err != nil {
		return BatchResult{}, fmt.Errorf("insert batch items: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BatchResult{}, fmt.Errorf("commit batch: %w", err)
	}
	return BatchResult{BatchID: batchID, IDs: ids}, nil
}

// replayBatch resolves an already stored batch for an idempotency key.
func (s *Store) replayBatch(ctx context.Context, key, hash string) (BatchResult, error) {
	var id string
	var stored *string
	if err := s.pool.QueryRow(ctx,
		`SELECT id::text, request_hash FROM batches WHERE idempotency_key = $1`, key).Scan(&id, &stored); err != nil {
		return BatchResult{}, fmt.Errorf("resolve batch idempotency key: %w", err)
	}
	if stored == nil || *stored != hash {
		return BatchResult{}, ErrIdempotencyConflict
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text FROM notifications WHERE batch_id = $1::uuid ORDER BY batch_position`, id)
	if err != nil {
		return BatchResult{}, fmt.Errorf("list batch items: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var nid string
		if err := rows.Scan(&nid); err != nil {
			return BatchResult{}, fmt.Errorf("scan batch item: %w", err)
		}
		ids = append(ids, nid)
	}
	if err := rows.Err(); err != nil {
		return BatchResult{}, fmt.Errorf("list batch items: %w", err)
	}
	return BatchResult{BatchID: id, IDs: ids, Replayed: true}, nil
}

// GetBatch returns a batch summary, or ErrNotFound.
func (s *Store) GetBatch(ctx context.Context, id string) (BatchSummary, error) {
	sum := BatchSummary{ID: id, Counts: map[domain.Status]int{
		domain.StatusPending: 0, domain.StatusProcessing: 0, domain.StatusSent: 0,
		domain.StatusFailed: 0, domain.StatusCancelled: 0,
	}}
	err := s.pool.QueryRow(ctx,
		`SELECT total, created_at FROM batches WHERE id = $1::uuid`, id).Scan(&sum.Total, &sum.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BatchSummary{}, ErrNotFound
	}
	if err != nil {
		return BatchSummary{}, fmt.Errorf("get batch: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT status, count(*) FROM notifications WHERE batch_id = $1::uuid GROUP BY status`, id)
	if err != nil {
		return BatchSummary{}, fmt.Errorf("count batch statuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var c int
		if err := rows.Scan(&st, &c); err != nil {
			return BatchSummary{}, fmt.Errorf("scan batch count: %w", err)
		}
		sum.Counts[domain.Status(st)] = c
	}
	if err := rows.Err(); err != nil {
		return BatchSummary{}, fmt.Errorf("count batch statuses: %w", err)
	}
	return sum, nil
}
