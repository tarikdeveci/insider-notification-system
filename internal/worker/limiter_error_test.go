package worker_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

var errRedisDown = errors.New("redis down")

var failingLimiter = funcLimiter(func(context.Context, string) (time.Duration, error) { return 0, errRedisDown })

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
