package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func TestMigration0002AddsTemplatesAndFK(t *testing.T) {
	dsn := testutil.PostgresDSN(t)
	if err := store.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("second run must be a no-op: %v", err)
	}
	pool := testutil.PoolFor(t, dsn)
	ctx := context.Background()

	var version int
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 2 || dirty {
		t.Fatalf("version=%d dirty=%v err=%v", version, dirty, err)
	}
	for _, c := range []string{"id", "name", "channel", "body", "created_at"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='templates' AND column_name=$1`, c).Scan(&n); err != nil || n != 1 {
			t.Errorf("missing column templates.%s (err=%v)", c, err)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO notifications (channel, recipient, content, correlation_id, template_id) VALUES ('sms','r','c','x', gen_random_uuid())`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("unknown template_id must violate the foreign key, got %v", err)
	}
}

func TestTemplateStoreCRUD(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	b, err := s.CreateTemplate(ctx, "b-welcome", domain.ChannelSMS, "Hi {{name}}")
	if err != nil || b.ID == "" || b.Name != "b-welcome" || b.Channel != domain.ChannelSMS || b.Body != "Hi {{name}}" || b.CreatedAt.IsZero() {
		t.Fatalf("create: %v %+v", err, b)
	}
	a, err := s.CreateTemplate(ctx, "a-promo", domain.ChannelEmail, "Sale")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTemplate(ctx, "a-promo", domain.ChannelPush, "x"); !errors.Is(err, store.ErrTemplateNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	got, err := s.GetTemplate(ctx, b.ID)
	if err != nil || got.ID != b.ID || got.Body != b.Body {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := s.GetTemplate(ctx, "3f2b8c1e-5a4d-4c6e-9b7a-1d2e3f4a5b6c"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	m, err := s.GetTemplates(ctx, []string{a.ID, b.ID, "3f2b8c1e-5a4d-4c6e-9b7a-1d2e3f4a5b6c"})
	if err != nil || len(m) != 2 || m[a.ID].Name != "a-promo" || m[b.ID].Name != "b-welcome" {
		t.Fatalf("batch get: %v %+v", err, m)
	}
	list, err := s.ListTemplates(ctx, 10)
	if err != nil || len(list) != 2 || list[0].Name != "a-promo" || list[1].Name != "b-welcome" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if list, err := s.ListTemplates(ctx, 1); err != nil || len(list) != 1 || list[0].Name != "a-promo" {
		t.Fatalf("list limit: %v %+v", err, list)
	}
}

func TestTemplateFieldsStoredAndRead(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	tpl, err := s.CreateTemplate(ctx, "t", domain.ChannelSMS, "Hi {{name}}")
	if err != nil {
		t.Fatal(err)
	}
	nn := sample(domain.ChannelSMS, "Hi Ada")
	nn.TemplateID = &tpl.ID
	nn.TemplateVars = map[string]string{"name": "Ada"}
	res, err := s.CreateNotification(ctx, store.CreateParams{New: nn, CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	check := func(label string, n domain.Notification) {
		t.Helper()
		if n.TemplateID == nil || *n.TemplateID != tpl.ID || n.TemplateVars["name"] != "Ada" || n.Content != "Hi Ada" {
			t.Errorf("%s: %+v", label, n)
		}
	}
	check("create", res.Notification)
	got, _ := s.GetNotification(ctx, res.Notification.ID)
	check("get", got)

	plain := mustCreate(t, s, domain.ChannelSMS, "plain")
	if plain.TemplateID != nil || plain.TemplateVars != nil {
		t.Errorf("content notification must have null template fields: %+v", plain)
	}

	batch, err := s.CreateBatch(ctx, store.CreateBatchParams{Items: []domain.NewNotification{nn, sample(domain.ChannelSMS, "no tpl")}, CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.GetNotification(ctx, batch.IDs[0])
	second, _ := s.GetNotification(ctx, batch.IDs[1])
	check("batch item", first)
	if second.TemplateID != nil || second.TemplateVars != nil {
		t.Errorf("batch item without template: %+v", second)
	}

	missing := "3f2b8c1e-5a4d-4c6e-9b7a-1d2e3f4a5b6c"
	bad := sample(domain.ChannelSMS, "x")
	bad.TemplateID = &missing
	if _, err := s.CreateNotification(ctx, store.CreateParams{New: bad, CorrelationID: "c"}); err == nil {
		t.Error("unknown template id must fail the FK")
	}
}
