package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// ListFilter selects and pages notifications, newest first.
type ListFilter struct {
	Status      *domain.Status
	Channel     *domain.Channel
	BatchID     *string
	CreatedFrom *time.Time // inclusive
	CreatedTo   *time.Time // inclusive
	Limit       int        // <=0 means 20; capped at 100
	Cursor      *Cursor
}

// ListNotifications returns one page ordered by created_at DESC, id DESC. The
// returned cursor is nil on the last page.
func (s *Store) ListNotifications(ctx context.Context, f ListFilter) ([]domain.Notification, *Cursor, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	var where []string
	var args []any
	// add appends a value and returns its placeholder number.
	add := func(v any) int {
		args = append(args, v)
		return len(args)
	}
	if f.Status != nil {
		where = append(where, fmt.Sprintf("status = $%d", add(string(*f.Status))))
	}
	if f.Channel != nil {
		where = append(where, fmt.Sprintf("channel = $%d", add(string(*f.Channel))))
	}
	if f.BatchID != nil {
		where = append(where, fmt.Sprintf("batch_id = $%d::uuid", add(*f.BatchID)))
	}
	if f.CreatedFrom != nil {
		where = append(where, fmt.Sprintf("created_at >= $%d", add(*f.CreatedFrom)))
	}
	if f.CreatedTo != nil {
		where = append(where, fmt.Sprintf("created_at <= $%d", add(*f.CreatedTo)))
	}
	if f.Cursor != nil {
		at := add(f.Cursor.CreatedAt)
		id := add(f.Cursor.ID)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d::uuid)", at, id))
	}

	q := "SELECT " + notificationCols + " FROM notifications"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", add(limit+1))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	items := make([]domain.Notification, 0, limit)
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan notification: %w", err)
		}
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list notifications: %w", err)
	}

	if len(items) <= limit {
		return items, nil, nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	return items, &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}
