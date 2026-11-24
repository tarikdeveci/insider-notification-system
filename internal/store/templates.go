package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

const templateCols = "id::text, name, channel, body, created_at"

func scanTemplate(row pgx.Row) (domain.Template, error) {
	var (
		t  domain.Template
		ch string
	)
	if err := row.Scan(&t.ID, &t.Name, &ch, &t.Body, &t.CreatedAt); err != nil {
		return domain.Template{}, err
	}
	t.Channel = domain.Channel(ch)
	return t, nil
}

// CreateTemplate stores a template. A repeated name returns ErrTemplateNameTaken.
func (s *Store) CreateTemplate(ctx context.Context, name string, channel domain.Channel, body string) (domain.Template, error) {
	t, err := scanTemplate(s.pool.QueryRow(ctx,
		`INSERT INTO templates (name, channel, body) VALUES ($1, $2, $3) RETURNING `+templateCols,
		name, string(channel), body))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.Template{}, ErrTemplateNameTaken
	}
	if err != nil {
		return domain.Template{}, fmt.Errorf("insert template: %w", err)
	}
	return t, nil
}

// GetTemplate returns one template by id, or ErrNotFound.
func (s *Store) GetTemplate(ctx context.Context, id string) (domain.Template, error) {
	t, err := scanTemplate(s.pool.QueryRow(ctx,
		`SELECT `+templateCols+` FROM templates WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Template{}, ErrNotFound
	}
	if err != nil {
		return domain.Template{}, fmt.Errorf("get template: %w", err)
	}
	return t, nil
}

// GetTemplates loads the templates with the given ids in one query. Unknown
// ids are simply absent from the result map.
func (s *Store) GetTemplates(ctx context.Context, ids []string) (map[string]domain.Template, error) {
	out := make(map[string]domain.Template, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+templateCols+` FROM templates WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("get templates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		out[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get templates: %w", err)
	}
	return out, nil
}

// ListTemplates returns at most limit templates ordered by name, then id.
func (s *Store) ListTemplates(ctx context.Context, limit int) ([]domain.Template, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+templateCols+` FROM templates ORDER BY name, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()
	out := []domain.Template{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	return out, nil
}
