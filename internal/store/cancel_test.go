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

func TestCancelPendingThenAgain(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	n := mustCreate(t, s, domain.ChannelSMS, "x")
	got, err := s.CancelNotification(ctx, n.ID)
	if err != nil || got.Status != domain.StatusCancelled {
		t.Fatalf("cancel: %v %+v", err, got)
	}
	if _, err := s.CancelNotification(ctx, n.ID); !errors.Is(err, store.ErrNotPending) {
		t.Fatalf("second cancel: want ErrNotPending, got %v", err)
	}
}

func TestCancelNotPendingAndMissing(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	n := mustCreate(t, s, domain.ChannelSMS, "x")
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status='processing' WHERE id=$1::uuid`, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelNotification(ctx, n.ID); !errors.Is(err, store.ErrNotPending) {
		t.Fatalf("processing: want ErrNotPending, got %v", err)
	}
	if _, err := s.CancelNotification(ctx, "123e4567-e89b-12d3-a456-426614174000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: want ErrNotFound, got %v", err)
	}
}

func TestCancelRacesClaimExactlyOneWins(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		n := mustCreate(t, s, domain.ChannelSMS, "race")
		var cancelled, claimed atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.CancelNotification(ctx, n.ID); err == nil {
				cancelled.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			tag, err := pool.Exec(ctx, `UPDATE notifications SET status='processing' WHERE id=$1::uuid AND status='pending'`, n.ID)
			if err == nil && tag.RowsAffected() == 1 {
				claimed.Add(1)
			}
		}()
		wg.Wait()
		if cancelled.Load()+claimed.Load() != 1 {
			t.Fatalf("round %d: cancelled=%d claimed=%d", round, cancelled.Load(), claimed.Load())
		}
	}
}

func TestCancelBatchOnlyPending(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	res, err := s.CreateBatch(ctx, store.CreateBatchParams{Items: batchItems(4), CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status='sent' WHERE id=$1::uuid`, res.IDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status='processing' WHERE id=$1::uuid`, res.IDs[1]); err != nil {
		t.Fatal(err)
	}
	n, err := s.CancelBatch(ctx, res.BatchID)
	if err != nil || n != 2 {
		t.Fatalf("cancelled=%d err=%v", n, err)
	}
	sum, err := s.GetBatch(ctx, res.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts[domain.StatusCancelled] != 2 || sum.Counts[domain.StatusSent] != 1 || sum.Counts[domain.StatusProcessing] != 1 {
		t.Fatalf("counts: %+v", sum.Counts)
	}
	if n, err := s.CancelBatch(ctx, res.BatchID); err != nil || n != 0 {
		t.Fatalf("second cancel: %d %v", n, err)
	}
	if _, err := s.CancelBatch(ctx, "123e4567-e89b-12d3-a456-426614174000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing batch: %v", err)
	}
}
