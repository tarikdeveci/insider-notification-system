package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

func TestCreateAndGet(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	res, err := s.CreateNotification(ctx, store.CreateParams{
		New:           domain.NewNotification{Channel: domain.ChannelSMS, Recipient: "+905551234567", Content: "hi", Priority: domain.PriorityHigh},
		CorrelationID: "corr-1",
	})
	if err != nil || res.Replayed {
		t.Fatalf("err=%v replayed=%v", err, res.Replayed)
	}
	n := res.Notification
	if n.ID == "" || n.Status != domain.StatusPending || n.Priority != domain.PriorityHigh || n.Attempts != 0 ||
		n.MaxAttempts != 8 || n.CorrelationID != "corr-1" || n.BatchID != nil {
		t.Fatalf("unexpected row: %+v", n)
	}
	got, err := s.GetNotification(ctx, n.ID)
	if err != nil || got.ID != n.ID || got.Content != "hi" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := s.GetNotification(ctx, "123e4567-e89b-12d3-a456-426614174000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	atts, err := s.ListAttempts(ctx, n.ID)
	if err != nil || len(atts) != 0 {
		t.Fatalf("attempts: %v %v", err, atts)
	}
}

func TestCreateWithoutKeyTwiceMakesTwoRows(t *testing.T) {
	s, pool := newStore(t)
	a := mustCreate(t, s, domain.ChannelSMS, "same")
	b := mustCreate(t, s, domain.ChannelSMS, "same")
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || n != 2 {
		t.Fatalf("ids %s %s rows %d", a.ID, b.ID, n)
	}
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	p := store.CreateParams{New: sample(domain.ChannelSMS, "hi"), IdempotencyKey: "k1", RequestHash: "h1", CorrelationID: "c"}
	first, err := s.CreateNotification(ctx, p)
	if err != nil || first.Replayed {
		t.Fatalf("first: %v %v", err, first.Replayed)
	}
	again, err := s.CreateNotification(ctx, p)
	if err != nil || !again.Replayed || again.Notification.ID != first.Notification.ID {
		t.Fatalf("replay: %v %+v", err, again)
	}
	p.RequestHash = "h2"
	if _, err := s.CreateNotification(ctx, p); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestCreateConcurrentSameKey(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	p := store.CreateParams{New: sample(domain.ChannelEmail, "x"), IdempotencyKey: "race", RequestHash: "h", CorrelationID: "c"}
	const n = 20
	var wg sync.WaitGroup
	var created atomic.Int32
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.CreateNotification(ctx, p)
			if err != nil {
				t.Error(err)
				return
			}
			if !res.Replayed {
				created.Add(1)
			}
			ids[i] = res.Notification.ID
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("exactly one creator expected, got %d", created.Load())
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("all callers must see the same id: %v", ids)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d", rows)
	}
}

func TestScheduledSetsNextAttemptAt(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	at := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	same := func(name string, got, want time.Time) {
		t.Helper()
		if !got.Truncate(time.Microsecond).Equal(want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
	}

	t.Run("single", func(t *testing.T) {
		nn := sample(domain.ChannelSMS, "later")
		nn.ScheduledAt = &at
		res, err := s.CreateNotification(ctx, store.CreateParams{New: nn, CorrelationID: "c"})
		if err != nil {
			t.Fatal(err)
		}
		n := res.Notification
		if n.ScheduledAt == nil {
			t.Fatal("scheduled_at not returned")
		}
		same("scheduled_at", *n.ScheduledAt, at)
		same("next_attempt_at", n.NextAttemptAt, at)
		got, err := s.GetNotification(ctx, n.ID)
		if err != nil || got.ScheduledAt == nil {
			t.Fatalf("get: %v %+v", err, got)
		}
		same("get scheduled_at", *got.ScheduledAt, at)

		plain := mustCreate(t, s, domain.ChannelSMS, "now")
		if plain.ScheduledAt != nil || time.Since(plain.NextAttemptAt) > time.Minute || plain.NextAttemptAt.After(time.Now().Add(time.Minute)) {
			t.Fatalf("unscheduled row: %+v", plain)
		}
	})

	t.Run("batch", func(t *testing.T) {
		items := batchItems(3)
		items[1].ScheduledAt = &at
		res, err := s.CreateBatch(ctx, store.CreateBatchParams{Items: items, CorrelationID: "c"})
		if err != nil {
			t.Fatal(err)
		}
		for i, id := range res.IDs {
			var sched *time.Time
			var next time.Time
			if err := pool.QueryRow(ctx, `SELECT scheduled_at, next_attempt_at FROM notifications WHERE id = $1::uuid`, id).Scan(&sched, &next); err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				if sched == nil {
					t.Fatal("item 1 lost scheduled_at")
				}
				same("batch scheduled_at", *sched, at)
				same("batch next_attempt_at", next, at)
				continue
			}
			if sched != nil || next.After(time.Now().Add(time.Minute)) || time.Since(next) > time.Minute {
				t.Fatalf("item %d: scheduled %v next %v", i, sched, next)
			}
		}
	})
}

func TestResolveReplay(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.ResolveReplay(ctx, "nokey", "h"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	first, err := s.CreateNotification(ctx, store.CreateParams{New: sample(domain.ChannelSMS, "x"), IdempotencyKey: "k", RequestHash: "h1", CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveReplay(ctx, "k", "h1")
	if err != nil || !got.Replayed || got.Notification.ID != first.Notification.ID {
		t.Fatalf("resolve: %v %+v", err, got)
	}
	if _, err := s.ResolveReplay(ctx, "k", "other"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("hash mismatch: %v", err)
	}

	if _, err := s.ResolveBatchReplay(ctx, "nobk", "h"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing batch key: %v", err)
	}
	b, err := s.CreateBatch(ctx, store.CreateBatchParams{Items: batchItems(2), IdempotencyKey: "bk", RequestHash: "bh", CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	br, err := s.ResolveBatchReplay(ctx, "bk", "bh")
	if err != nil || !br.Replayed || br.BatchID != b.BatchID || len(br.IDs) != 2 {
		t.Fatalf("resolve batch: %v %+v", err, br)
	}
	if _, err := s.ResolveBatchReplay(ctx, "bk", "other"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("batch hash mismatch: %v", err)
	}
}
