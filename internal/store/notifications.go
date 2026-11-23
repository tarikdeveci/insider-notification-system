package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

// CreateParams carries one notification to insert plus its idempotency data.
type CreateParams struct {
	New            domain.NewNotification
	IdempotencyKey string // empty means none
	RequestHash    string
	CorrelationID  string
}

// CreateResult is the outcome of CreateNotification.
type CreateResult struct {
	Notification domain.Notification
	Replayed     bool
}

// CreateNotification inserts a notification, or resolves an existing one when
// the idempotency key was already used. A key reused with a different request
// hash returns ErrIdempotencyConflict.
func (s *Store) CreateNotification(ctx context.Context, p CreateParams) (CreateResult, error) {
	var key any // nil never conflicts on the unique index
	if p.IdempotencyKey != "" {
		key = p.IdempotencyKey
	}
	n, err := scanNotification(s.pool.QueryRow(ctx,
		`INSERT INTO notifications (idempotency_key, request_hash, channel, recipient, content, priority, correlation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING `+notificationCols,
		key, p.RequestHash, string(p.New.Channel), p.New.Recipient, p.New.Content, int16(p.New.Priority), p.CorrelationID))
	if err == nil {
		return CreateResult{Notification: n}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || p.IdempotencyKey == "" {
		return CreateResult{}, fmt.Errorf("insert notification: %w", err)
	}

	var stored *string
	n, err = scanNotification(s.pool.QueryRow(ctx,
		`SELECT `+notificationCols+`, request_hash FROM notifications WHERE idempotency_key = $1`,
		p.IdempotencyKey), &stored)
	if err != nil {
		return CreateResult{}, fmt.Errorf("resolve idempotency key: %w", err)
	}
	if stored == nil || *stored != p.RequestHash {
		return CreateResult{}, ErrIdempotencyConflict
	}
	return CreateResult{Notification: n, Replayed: true}, nil
}

// GetNotification returns one notification by id, or ErrNotFound.
func (s *Store) GetNotification(ctx context.Context, id string) (domain.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx,
		`SELECT `+notificationCols+` FROM notifications WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return n, nil
}

// ListAttempts returns the delivery attempts of a notification in insertion
// order. The slice is non-nil even when there are none.
func (s *Store) ListAttempts(ctx context.Context, id string) ([]domain.Attempt, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT attempt_no, started_at, finished_at, http_status, error, duration_ms
		 FROM notification_attempts WHERE notification_id = $1::uuid ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()
	out := []domain.Attempt{}
	for rows.Next() {
		var a domain.Attempt
		if err := rows.Scan(&a.AttemptNo, &a.StartedAt, &a.FinishedAt, &a.HTTPStatus, &a.Error, &a.DurationMS); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	return out, nil
}
