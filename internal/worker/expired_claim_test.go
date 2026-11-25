package worker_test

import (
	"context"
	"strings"
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

// ageQueue hands out one live row once and records every FinishAttempt.
// StartAttempt reports startedAt as the database clock at the attempt start.
type ageQueue struct {
	row       store.QueueRow
	startedAt time.Time
	calls     atomic.Int32
	mu        sync.Mutex
	finished  []store.FinishParams
}

func (q *ageQueue) ClaimWithAge(context.Context, domain.Channel, int, string, time.Duration) ([]store.QueueRow, error) {
	if q.calls.Add(1) == 1 {
		return []store.QueueRow{q.row}, nil
	}
	return nil, nil
}
func (q *ageQueue) StartAttempt(context.Context, string, string) (int, time.Time, error) {
	return 1, q.startedAt, nil
}
func (q *ageQueue) FinishAttempt(_ context.Context, p store.FinishParams) error {
	q.mu.Lock()
	q.finished = append(q.finished, p)
	q.mu.Unlock()
	return nil
}
func (q *ageQueue) Release(context.Context, string, string, time.Duration) error { return nil }
func (q *ageQueue) Reap(context.Context, time.Duration) ([]store.ReapedRow, error) {
	return nil, nil
}

func (q *ageQueue) waitFinished(t *testing.T) store.FinishParams {
	t.Helper()
	waitFor(t, "attempt finished", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.finished) == 1
	})
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.finished[0]
}

// The claim checks the age, but the limiter wait comes after it. A row the
// database clock puts past MAX_DELIVERY_AGE when its attempt starts fails as
// expired and is never sent.
func TestRowThatExpiresDuringTheLimiterWaitIsNeverSent(t *testing.T) {
	dbNow := time.Now()
	q := &ageQueue{
		row: store.QueueRow{
			ID: "44444444-4444-4444-8444-444444444444", Channel: domain.ChannelSMS, Recipient: "r", Content: "c",
			MaxAttempts: 3, LeaseToken: "t", CreatedAt: dbNow.Add(-24*time.Hour - time.Second),
		},
		startedAt: dbNow,
	}
	obs := &recObserver{}
	var sends atomic.Int32
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		sends.Add(1)
		return okSender(ctx, m)
	})
	start(t, worker.New(q, sender, okLimiter, testSettings(), worker.WithRand(zeroRand), worker.WithObserver(obs), worker.WithChannels(domain.ChannelSMS)))
	fp := q.waitFinished(t)
	if sends.Load() != 0 {
		t.Fatalf("sent %d times after the max delivery age", sends.Load())
	}
	if fp.Action != store.FinishFail || !strings.HasPrefix(fp.LastError, worker.ReasonExpired+": ") || fp.HTTPStatus != 0 {
		t.Fatalf("finish %+v, want a failed expired attempt with no HTTP status", fp)
	}
	waitFor(t, "expired counted", func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		return len(obs.failed) == 1 && obs.failed[0] == "sms:expired"
	})
}

// Expiry after an attempt is judged on the database clock, like the claim and
// the reaper: a worker clock five minutes fast must not expire a row the
// database still gives two minutes.
func TestExpiryAfterAnAttemptUsesTheDatabaseClock(t *testing.T) {
	dbNow := time.Now()
	q := &ageQueue{
		row: store.QueueRow{
			ID: "55555555-5555-4555-8555-555555555555", Channel: domain.ChannelSMS, Recipient: "r", Content: "c",
			MaxAttempts: 3, LeaseToken: "t", CreatedAt: dbNow.Add(-24*time.Hour + 2*time.Minute),
		},
		startedAt: dbNow,
	}
	sender := funcSender(func(context.Context, provider.Message) provider.Result {
		return provider.Result{Class: provider.Retryable, ErrCode: provider.Code5xx, HTTPStatus: 500}
	})
	fast := func() time.Time { return time.Now().Add(5 * time.Minute) }
	start(t, worker.New(q, sender, okLimiter, testSettings(), worker.WithRand(zeroRand), worker.WithClock(fast), worker.WithChannels(domain.ChannelSMS)))
	if fp := q.waitFinished(t); fp.Action != store.FinishRetry {
		t.Fatalf("finish action %v (last_error %q), want a retry", fp.Action, fp.LastError)
	}
}
