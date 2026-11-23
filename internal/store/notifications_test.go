package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

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
	pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications`).Scan(&n)
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
	pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("rows = %d", rows)
	}
}
