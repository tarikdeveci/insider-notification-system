package store

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

// Sentinel errors returned by store methods.
var (
	ErrNotFound            = errors.New("not found")
	ErrNotPending          = errors.New("notification is not pending")
	ErrNotFailed           = errors.New("notification is not failed")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different payload")
)

// Store is the Postgres-backed persistence layer.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store on pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// notificationCols is the standard select list read by scanNotification.
const notificationCols = "id::text, batch_id::text, channel, recipient, content, priority, status, attempts, max_attempts, next_attempt_at, provider_message_id, last_error, correlation_id, created_at, updated_at, sent_at, scheduled_at"

// scanNotification scans the notificationCols columns, then any extra
// destinations, from a pgx.Row or the current pgx.Rows row.
func scanNotification(row pgx.Row, extra ...any) (domain.Notification, error) {
	var (
		n        domain.Notification
		channel  string
		status   string
		priority int16
		sentAt   *time.Time
	)
	dest := []any{
		&n.ID, &n.BatchID, &channel, &n.Recipient, &n.Content, &priority, &status,
		&n.Attempts, &n.MaxAttempts, &n.NextAttemptAt, &n.ProviderMessageID, &n.LastError,
		&n.CorrelationID, &n.CreatedAt, &n.UpdatedAt, &sentAt, &n.ScheduledAt,
	}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return domain.Notification{}, err
	}
	n.Channel = domain.Channel(channel)
	n.Status = domain.Status(status)
	n.Priority = domain.Priority(priority)
	n.SentAt = sentAt
	return n, nil
}
