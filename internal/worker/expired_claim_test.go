package worker_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

// expiredQueue hands out one row the claim already failed as expired, then
// nothing. It records the max age the engine passes.
type expiredQueue struct {
	calls   atomic.Int32
	started atomic.Int32
	mu      sync.Mutex
	ages    []time.Duration
}

func (q *expiredQueue) ClaimWithAge(_ context.Context, _ domain.Channel, _ int, _ string, maxAge time.Duration) ([]store.QueueRow, error) {
	q.mu.Lock()
	q.ages = append(q.ages, maxAge)
	q.mu.Unlock()
	if q.calls.Add(1) == 1 {
		return []store.QueueRow{{
			ID: "33333333-3333-4333-8333-333333333333", Channel: domain.ChannelSMS, Recipient: "r", Content: "c",
			MaxAttempts: 3, CreatedAt: time.Now().Add(-25 * time.Hour), Expired: true,
		}}, nil
	}
	return nil, nil
}

func (q *expiredQueue) StartAttempt(context.Context, string, string) (int, time.Time, error) {
	q.started.Add(1)
	return 1, time.Now(), nil
}
func (q *expiredQueue) FinishAttempt(context.Context, store.FinishParams) error      { return nil }
func (q *expiredQueue) Release(context.Context, string, string, time.Duration) error { return nil }
func (q *expiredQueue) Reap(context.Context, time.Duration) ([]store.ReapedRow, error) {
	return nil, nil
}

// A row the claim expired is counted as failed with reason expired and is
// never sent; the engine passes its MaxDeliveryAge to every claim.
func TestExpiredClaimIsCountedAndNeverSent(t *testing.T) {
	q := &expiredQueue{}
	obs := &recObserver{}
	var sends atomic.Int32
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		sends.Add(1)
		return okSender(ctx, m)
	})
	set := testSettings()
	set.MaxDeliveryAge = 7 * time.Hour
	start(t, worker.New(q, sender, okLimiter, set, worker.WithRand(zeroRand), worker.WithObserver(obs), worker.WithChannels(domain.ChannelSMS)))
	waitFor(t, "expired row counted", func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		return len(obs.failed) == 1
	})
	obs.mu.Lock()
	failed := append([]string(nil), obs.failed...)
	obs.mu.Unlock()
	if failed[0] != "sms:expired" {
		t.Fatalf("failed = %v, want [sms:expired]", failed)
	}
	if sends.Load() != 0 || q.started.Load() != 0 {
		t.Fatalf("sends = %d, starts = %d, want none", sends.Load(), q.started.Load())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, a := range q.ages {
		if a != 7*time.Hour {
			t.Fatalf("claim max age = %s, want 7h", a)
		}
	}
}
