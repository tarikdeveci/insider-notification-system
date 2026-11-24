package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability/tracectx"
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
	vars, err := varsJSON(p.New.TemplateVars)
	if err != nil {
		return CreateResult{}, err
	}
	// The row keeps the caller's span (the HTTP request), not the insert span, so
	// one trace covers the request and the later delivery.
	traceParent := tracectx.TraceParent(ctx)
	ctx, span := tracectx.Start(ctx, "db.insert_notification", trace.WithAttributes(
		attribute.String("db.system.name", "postgresql"),
		attribute.String("notification.channel", string(p.New.Channel)),
		attribute.Int("notification.priority", int(p.New.Priority)),
	))
	defer span.End()
	n, err := scanNotification(s.pool.QueryRow(ctx,
		`INSERT INTO notifications (idempotency_key, request_hash, channel, recipient, content, priority, correlation_id, scheduled_at, next_attempt_at, template_id, template_vars, trace_parent)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::timestamptz, COALESCE($8::timestamptz, now()), $9::uuid, $10::jsonb, NULLIF($11::text, ''))
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING `+notificationCols,
		key, p.RequestHash, string(p.New.Channel), p.New.Recipient, p.New.Content, int16(p.New.Priority), p.CorrelationID, p.New.ScheduledAt, p.New.TemplateID, vars, traceParent))
	if err == nil {
		span.SetAttributes(attribute.String("notification.id", n.ID))
		return CreateResult{Notification: n}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || p.IdempotencyKey == "" {
		span.SetStatus(codes.Error, "insert failed")
		return CreateResult{}, fmt.Errorf("insert notification: %w", err)
	}
	span.SetAttributes(attribute.Bool("notification.replayed", true))

	return s.ResolveReplay(ctx, p.IdempotencyKey, p.RequestHash)
}

// ResolveReplay returns the notification stored under an idempotency key with
// Replayed set, without inserting. It returns ErrNotFound when no row has the
// key and ErrIdempotencyConflict when the stored request hash differs.
func (s *Store) ResolveReplay(ctx context.Context, key, hash string) (CreateResult, error) {
	var stored *string
	n, err := scanNotification(s.pool.QueryRow(ctx,
		`SELECT `+notificationCols+`, request_hash FROM notifications WHERE idempotency_key = $1`,
		key), &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreateResult{}, ErrNotFound
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("resolve idempotency key: %w", err)
	}
	if stored == nil || *stored != hash {
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

// ListAttempts returns the delivery attempts of a notification in write (seq)
// order. attempt_no is the notification's attempts counter at write time and
// can repeat after a 429 refund; seq is the unambiguous order. The slice is
// non-nil even when there are none.
func (s *Store) ListAttempts(ctx context.Context, id string) ([]domain.Attempt, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT attempt_no, started_at, finished_at, http_status, error, duration_ms
		 FROM notification_attempts WHERE notification_id = $1::uuid ORDER BY seq`, id)
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

// varsJSON encodes template vars for a jsonb parameter; nil means SQL NULL.
func varsJSON(v map[string]string) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode template vars: %w", err)
	}
	return string(b), nil
}
