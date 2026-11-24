package worker_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

var errRedisDown = errors.New("redis down")

var failingLimiter = funcLimiter(func(context.Context, string) (time.Duration, error) { return 0, errRedisDown })

// nonEmptyQueue counts the Claim calls that returned rows.
type nonEmptyQueue struct {
	worker.Queue
	claims atomic.Int32
}

func (q *nonEmptyQueue) ClaimWithAge(ctx context.Context, ch domain.Channel, n int, token string, maxAge time.Duration) ([]store.QueueRow, error) {
	rows, err := q.Queue.ClaimWithAge(ctx, ch, n, token, maxAge)
	if len(rows) > 0 {
		q.claims.Add(1)
	}
	return rows, err
}

// A limiter that always errors (Redis outage) must not turn into a hot loop of
// claim and release: the loop backs off and the released rows wait.
func TestLimiterOutageBacksOffClaims(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		e.create(t, domain.ChannelSMS, "c")
	}
	q := &nonEmptyQueue{Queue: e.st}
	logs := &syncBuf{}
	start(t, worker.New(q, funcSender(okSender), failingLimiter, testSettings(), worker.WithRand(zeroRand),
		worker.WithChannels(domain.ChannelSMS), worker.WithLogger(observability.NewLogger(logs, "info"))))
	time.Sleep(700 * time.Millisecond)
	n := q.claims.Load()
	if n < 1 || n > 8 {
		t.Fatalf("%d non-empty claims in 700ms during a limiter outage, want 1 to 8", n)
	}
	if got := strings.Count(logs.String(), `"level":"WARN"`); got > 2 {
		t.Fatalf("limiter outage logged %d warnings, want it collapsed to one per outage", got)
	}
}

// Limiter errors say nothing about how long a row waited, so they must not feed
// the wait histogram with zeros.
func TestLimiterErrorDoesNotRecordRateLimitWait(t *testing.T) {
	e := newEnv(t)
	e.create(t, domain.ChannelSMS, "c")
	logs := &syncBuf{}
	obs := &recObserver{}
	start(t, worker.New(e.st, funcSender(okSender), failingLimiter, testSettings(), worker.WithRand(zeroRand),
		worker.WithChannels(domain.ChannelSMS), worker.WithObserver(obs),
		worker.WithLogger(observability.NewLogger(logs, "debug"))))
	waitFor(t, "limiter error released the row", func() bool { return strings.Contains(logs.String(), "ratelimit_error") })
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.waits != 0 {
		t.Fatalf("RateLimitWait recorded %d times for limiter errors, want 0", obs.waits)
	}
}
