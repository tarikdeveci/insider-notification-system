package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

// CancelNotification moves a pending notification to cancelled with a single
// conditional UPDATE. It returns ErrNotFound when the id does not exist and
// ErrNotPending when the notification exists but is no longer pending.
func (s *Store) CancelNotification(ctx context.Context, id string) (domain.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx,
		`UPDATE notifications SET status = 'cancelled', updated_at = now()
		 WHERE id = $1::uuid AND status = 'pending'
		 RETURNING `+notificationCols, id))
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, fmt.Errorf("cancel notification: %w", err)
	}
	var one int
	err = s.pool.QueryRow(ctx, `SELECT 1 FROM notifications WHERE id = $1::uuid`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("check notification: %w", err)
	}
	return domain.Notification{}, ErrNotPending
}

// RetryNotification requeues a failed notification with a single conditional
// UPDATE: status pending, attempts 0, due now, lease cleared. last_error,
// scheduled_at and max_attempts are kept. It returns ErrNotFound when the id
// does not exist and ErrNotFailed when the notification is not failed.
func (s *Store) RetryNotification(ctx context.Context, id string) (domain.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx,
		`UPDATE notifications SET status = 'pending', attempts = 0, next_attempt_at = now(), requeued_at = now(),
		   lease_token = NULL, locked_until = NULL, updated_at = now()
		 WHERE id = $1::uuid AND status = 'failed'
		 RETURNING `+notificationCols, id))
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, fmt.Errorf("retry notification: %w", err)
	}
	var one int
	err = s.pool.QueryRow(ctx, `SELECT 1 FROM notifications WHERE id = $1::uuid`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("check notification: %w", err)
	}
	return domain.Notification{}, ErrNotFailed
}

// CancelBatch cancels every pending notification of a batch and returns how
// many rows changed (possibly 0). It returns ErrNotFound when the batch does
// not exist.
func (s *Store) CancelBatch(ctx context.Context, batchID string) (int, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM batches WHERE id = $1::uuid`, batchID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("check batch: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE notifications SET status = 'cancelled', updated_at = now()
		 WHERE batch_id = $1::uuid AND status = 'pending'`, batchID)
	if err != nil {
		return 0, fmt.Errorf("cancel batch: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
