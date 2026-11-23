package worker_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/ratelimit"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

// atomicClock is a test clock safe for the engine goroutines.
type atomicClock struct{ ns atomic.Int64 }

func newAtomicClock() *atomicClock {
	c := &atomicClock{}
	c.ns.Store(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *atomicClock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *atomicClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// brkQueue is a Queue with a scripted Claim that records every call.
type brkQueue struct {
	mu       sync.Mutex
	onClaim  func(n int) []store.QueueRow
	claims   []int
	starts   int
	released []string
	next     int
}

func (q *brkQueue) Claim(_ context.Context, _ domain.Channel, n int, _ string) ([]store.QueueRow, error) {
	q.mu.Lock()
	q.claims = append(q.claims, n)
	q.mu.Unlock()
	return q.onClaim(n), nil
}
func (q *brkQueue) StartAttempt(context.Context, string, string) (int, time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.starts++
	return 1, time.Now(), nil
}
func (q *brkQueue) FinishAttempt(context.Context, store.FinishParams) error { return nil }
func (q *brkQueue) Release(_ context.Context, id, _ string) error {
	q.mu.Lock()
	q.released = append(q.released, id)
	q.mu.Unlock()
	return nil
}
func (q *brkQueue) Reap(context.Context, time.Duration) ([]store.ReapedRow, error) { return nil, nil }

func (q *brkQueue) claimNs() []int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.claims)
}
func (q *brkQueue) startCount() int { q.mu.Lock(); defer q.mu.Unlock(); return q.starts }
func (q *brkQueue) releasedIDs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.released)
}

// rows makes n fresh sms rows with unique ids.
func (q *brkQueue) rows(n int) []store.QueueRow {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]store.QueueRow, n)
	for i := range out {
		q.next++
		out[i] = store.QueueRow{
			ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", q.next), Channel: domain.ChannelSMS,
			MaxAttempts: 8, CreatedAt: time.Now(), LeaseToken: "tok", CorrelationID: "c",
		}
	}
	return out
}

func breakerSettings(threshold int) worker.Settings {
	s := testSettings()
	s.BreakerThreshold = threshold
	s.BreakerOpenFor = 30 * time.Second
	return s
}

func retryableSender(context.Context, provider.Message) provider.Result {
	return provider.Result{Class: provider.Retryable, HTTPStatus: 503, ErrCode: "http_503"}
}

func TestOpenStopsClaimingFor30sThenHalfOpen(t *testing.T) {
	clk := newAtomicClock()
	q := &brkQueue{}
	q.onClaim = q.rows
	obs := &recObserver{}
	start(t, worker.New(q, funcSender(retryableSender), okLimiter, breakerSettings(3),
		worker.WithRand(zeroRand), worker.WithClock(clk.now), worker.WithObserver(obs), worker.WithChannels(domain.ChannelSMS)))

	waitFor(t, "breaker open", func() bool { return slices.Contains(obs.stateList(), "sms:1") })
	time.Sleep(100 * time.Millisecond) // let a Claim that raced the opening finish
	before := len(q.claimNs())
	time.Sleep(200 * time.Millisecond) // many idle polls (IdleMax 20ms); the clock has not moved
	if after := len(q.claimNs()); after != before {
		t.Fatalf("Claim called %d more times while the breaker was open and the clock before openedAt+30s", after-before)
	}

	clk.advance(30 * time.Second)
	waitFor(t, "one probe Claim of limit 1", func() bool {
		ns := q.claimNs()
		return len(ns) > before && ns[before] == 1
	})
}

func TestBreakerPausesDoNotConsumeAttempts(t *testing.T) {
	q := &brkQueue{}
	var once sync.Once
	q.onClaim = func(n int) []store.QueueRow {
		var out []store.QueueRow
		once.Do(func() { out = q.rows(4) })
		return out
	}
	// The first Acquire passes at once, the others wait until the breaker is open.
	gate := make(chan struct{})
	var acquires atomic.Int32
	lim := funcLimiter(func(ctx context.Context, _ string) (time.Duration, error) {
		if acquires.Add(1) > 1 {
			select {
			case <-gate:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return 0, nil
	})
	var sends atomic.Int32
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		sends.Add(1)
		return retryableSender(ctx, m)
	})
	obs := &recObserver{}
	logs := &syncBuf{}
	start(t, worker.New(q, sender, lim, breakerSettings(1), worker.WithRand(zeroRand), worker.WithObserver(obs),
		worker.WithChannels(domain.ChannelSMS), worker.WithLogger(observability.NewLogger(logs, "info"))))

	waitFor(t, "breaker open", func() bool { return slices.Contains(obs.stateList(), "sms:1") })
	close(gate)
	waitFor(t, "three rows released", func() bool { return len(q.releasedIDs()) == 3 })
	if got := q.startCount(); got != 1 {
		t.Fatalf("StartAttempt calls = %d, want 1 (only the row sent before the breaker opened)", got)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("Send calls = %d, want 1", got)
	}
	waitFor(t, "release logs", func() bool { return strings.Count(logs.String(), `"reason":"breaker_open"`) == 3 })
}

func TestEngineBreakerObserverSeesOpenHalfOpenClosed(t *testing.T) {
	clk := newAtomicClock()
	q := &brkQueue{}
	q.onClaim = q.rows
	var healthy atomic.Bool
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		if healthy.Load() {
			return provider.Result{Class: provider.Success, MessageID: "m", HTTPStatus: 202}
		}
		return retryableSender(ctx, m)
	})
	obs := &recObserver{}
	start(t, worker.New(q, sender, okLimiter, breakerSettings(2),
		worker.WithRand(zeroRand), worker.WithClock(clk.now), worker.WithObserver(obs), worker.WithChannels(domain.ChannelSMS)))

	waitFor(t, "open", func() bool { return slices.Equal(obs.stateList(), []string{"sms:1"}) })
	healthy.Store(true)
	clk.advance(30 * time.Second)
	want := []string{"sms:1", "sms:2", "sms:0"}
	waitFor(t, "half open then closed", func() bool { return slices.Equal(obs.stateList(), want) })
	time.Sleep(100 * time.Millisecond)
	if got := obs.stateList(); !slices.Equal(got, want) {
		t.Fatalf("states = %v, want exactly open, half open, closed", got)
	}
}

func TestProbeEndedWithoutSendFreesSlotAtEngineLevel(t *testing.T) {
	clk := newAtomicClock()
	q := &brkQueue{}
	q.onClaim = q.rows
	var healthy, limiterDenies atomic.Bool
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		if healthy.Load() {
			return provider.Result{Class: provider.Success, MessageID: "m", HTTPStatus: 202}
		}
		return retryableSender(ctx, m)
	})
	lim := funcLimiter(func(context.Context, string) (time.Duration, error) {
		if limiterDenies.Load() {
			return 0, ratelimit.ErrWaitExceeded
		}
		return 0, nil
	})
	obs := &recObserver{}
	start(t, worker.New(q, sender, lim, breakerSettings(1),
		worker.WithRand(zeroRand), worker.WithClock(clk.now), worker.WithObserver(obs), worker.WithChannels(domain.ChannelSMS)))

	waitFor(t, "open", func() bool { return slices.Contains(obs.stateList(), "sms:1") })
	// The probe row is released by the limiter before any send: the probe slot must be freed.
	limiterDenies.Store(true)
	healthy.Store(true)
	clk.advance(30 * time.Second)
	waitFor(t, "probe row released unsent", func() bool { return len(q.releasedIDs()) > 0 })
	limiterDenies.Store(false)
	waitFor(t, "a later probe closes the breaker", func() bool {
		return slices.Equal(obs.stateList(), []string{"sms:1", "sms:2", "sms:0"})
	})
}
