package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

func batchItems(n int) []domain.NewNotification {
	out := make([]domain.NewNotification, n)
	for i := range out {
		out[i] = sample(domain.ChannelSMS, fmt.Sprintf("msg-%d", i))
	}
	return out
}

func TestCreateBatchKeepsRequestOrder(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	res, err := s.CreateBatch(ctx, store.CreateBatchParams{Items: batchItems(5), CorrelationID: "c"})
	if err != nil || res.Replayed || res.BatchID == "" || len(res.IDs) != 5 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	for i, id := range res.IDs {
		n, err := s.GetNotification(ctx, id)
		if err != nil || n.Content != fmt.Sprintf("msg-%d", i) || n.BatchID == nil || *n.BatchID != res.BatchID {
			t.Fatalf("item %d: %v %+v", i, err, n)
		}
	}
}

func TestCreateBatchAllowsIdenticalItems(t *testing.T) {
	s, _ := newStore(t)
	same := sample(domain.ChannelPush, "dup")
	res, err := s.CreateBatch(context.Background(), store.CreateBatchParams{Items: []domain.NewNotification{same, same, same}, CorrelationID: "c"})
	if err != nil || len(res.IDs) != 3 || res.IDs[0] == res.IDs[1] {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

func TestCreateBatchReplayAndConflict(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	p := store.CreateBatchParams{Items: batchItems(3), IdempotencyKey: "bk", RequestHash: "h1", CorrelationID: "c"}
	first, err := s.CreateBatch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateBatch(ctx, p)
	if err != nil || !again.Replayed || again.BatchID != first.BatchID {
		t.Fatalf("replay: %v %+v", err, again)
	}
	for i := range first.IDs {
		if again.IDs[i] != first.IDs[i] {
			t.Fatalf("replay order differs at %d", i)
		}
	}
	p.RequestHash = "h2"
	if _, err := s.CreateBatch(ctx, p); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("replay must not insert rows, have %d", rows)
	}
}

func TestCreateBatchConcurrentSameKey(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	p := store.CreateBatchParams{Items: batchItems(3), IdempotencyKey: "race", RequestHash: "h", CorrelationID: "c"}
	var wg sync.WaitGroup
	var created atomic.Int32
	const n = 10
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.CreateBatch(ctx, p)
			if err != nil {
				t.Error(err)
				return
			}
			if !res.Replayed {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	var batches, notifs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&notifs); err != nil {
		t.Fatal(err)
	}
	if created.Load() != 1 || batches != 1 || notifs != 3 {
		t.Fatalf("created=%d batches=%d notifications=%d", created.Load(), batches, notifs)
	}
}

func TestGetBatchCounts(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	res, err := s.CreateBatch(ctx, store.CreateBatchParams{Items: batchItems(3), CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status='sent' WHERE id=$1::uuid`, res.IDs[0]); err != nil {
		t.Fatal(err)
	}
	sum, err := s.GetBatch(ctx, res.BatchID)
	if err != nil || sum.Total != 3 || sum.Counts[domain.StatusPending] != 2 || sum.Counts[domain.StatusSent] != 1 || sum.Counts[domain.StatusFailed] != 0 {
		t.Fatalf("err=%v sum=%+v", err, sum)
	}
	if len(sum.Counts) != 5 {
		t.Fatalf("counts must list every status, got %v", sum.Counts)
	}
	if _, err := s.GetBatch(ctx, "123e4567-e89b-12d3-a456-426614174000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
