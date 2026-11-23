package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

func setCreatedAt(t *testing.T, pool *pgxpool.Pool, id string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE notifications SET created_at=$2 WHERE id=$1::uuid`, id, at); err != nil {
		t.Fatal(err)
	}
}

func TestListPaginatesWithoutOverlapOrGaps(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	var ids []string
	for i := 0; i < 5; i++ {
		n := mustCreate(t, s, domain.ChannelSMS, "m")
		// rows 3 and 4 share a timestamp to exercise the id tiebreak
		at := base.Add(time.Duration(min(i, 3)) * time.Second)
		setCreatedAt(t, pool, n.ID, at)
		ids = append(ids, n.ID)
	}
	var seen []string
	var cur *store.Cursor
	for page := 0; page < 5; page++ {
		items, next, err := s.ListNotifications(ctx, store.ListFilter{Limit: 2, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range items {
			seen = append(seen, n.ID)
		}
		if next == nil {
			break
		}
		cur = next
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d rows: %v", len(seen), seen)
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(uniq) != 5 {
		t.Fatalf("duplicates across pages: %v", seen)
	}
	// newest first: the oldest row (index 0) must be last
	if seen[4] != ids[0] {
		t.Errorf("order wrong: last=%s want %s", seen[4], ids[0])
	}
}

func TestListFilters(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	a := mustCreate(t, s, domain.ChannelSMS, "a")
	b := mustCreate(t, s, domain.ChannelEmail, "b")
	c := mustCreate(t, s, domain.ChannelEmail, "c")
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status='sent' WHERE id=$1::uuid`, c.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	setCreatedAt(t, pool, a.ID, now.Add(-3*time.Hour))
	setCreatedAt(t, pool, b.ID, now.Add(-2*time.Hour))
	setCreatedAt(t, pool, c.ID, now.Add(-1*time.Hour))

	count := func(f store.ListFilter) int {
		items, _, err := s.ListNotifications(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	email, sent := domain.ChannelEmail, domain.StatusSent
	from, to := now.Add(-150*time.Minute), now.Add(-90*time.Minute)
	if got := count(store.ListFilter{Channel: &email}); got != 2 {
		t.Errorf("channel filter: %d", got)
	}
	if got := count(store.ListFilter{Status: &sent}); got != 1 {
		t.Errorf("status filter: %d", got)
	}
	if got := count(store.ListFilter{Status: &sent, Channel: &email}); got != 1 {
		t.Errorf("status+channel: %d", got)
	}
	if got := count(store.ListFilter{CreatedFrom: &from, CreatedTo: &to}); got != 1 {
		t.Errorf("date range: %d", got)
	}
	if got := count(store.ListFilter{Limit: 1000}); got != 3 {
		t.Errorf("small set under cap: %d", got)
	}
}

func TestListLimitCapsAtHundred(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO notifications (channel, recipient, content, priority, correlation_id)
		 SELECT 'sms', '+905551234567', 'm', 1, 'test' FROM generate_series(1, 105)`); err != nil {
		t.Fatal(err)
	}
	items, next, err := s.ListNotifications(ctx, store.ListFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 100 || next == nil {
		t.Fatalf("got %d rows, next=%v; want 100 rows and a cursor", len(items), next)
	}
	if def, _, err := s.ListNotifications(ctx, store.ListFilter{}); err != nil || len(def) != 20 {
		t.Fatalf("default limit: %d %v", len(def), err)
	}
}

func TestListBatchFilter(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	n := mustCreate(t, s, domain.ChannelSMS, "x")
	mustCreate(t, s, domain.ChannelSMS, "y")
	var batch string
	if err := pool.QueryRow(ctx, `INSERT INTO batches (total) VALUES (1) RETURNING id::text`).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET batch_id=$2::uuid WHERE id=$1::uuid`, n.ID, batch); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.ListNotifications(ctx, store.ListFilter{BatchID: &batch})
	if err != nil || len(items) != 1 || items[0].ID != n.ID {
		t.Fatalf("batch filter: %v %d", err, len(items))
	}
}
