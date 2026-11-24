package worker_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

// partialQueue fails its first Claim after it already moved one row to
// processing, which is what a failed later priority probe looks like.
type partialQueue struct {
	calls    atomic.Int32
	finished atomic.Int32
}

func (q *partialQueue) claim() ([]store.QueueRow, error) {
	if q.calls.Add(1) == 1 {
		row := store.QueueRow{
			ID: "11111111-1111-4111-8111-111111111111", Channel: domain.ChannelSMS, Recipient: "r", Content: "c",
			MaxAttempts: 3, CreatedAt: time.Now(), LeaseToken: "22222222-2222-4222-8222-222222222222",
		}
		return []store.QueueRow{row}, errors.New("claim: second probe failed")
	}
	return nil, nil
}

func (q *partialQueue) Claim(context.Context, domain.Channel, int, string) ([]store.QueueRow, error) {
	return q.claim()
}

func (q *partialQueue) StartAttempt(context.Context, string, string) (int, time.Time, error) {
	return 1, time.Now(), nil
}

func (q *partialQueue) FinishAttempt(context.Context, store.FinishParams) error {
	q.finished.Add(1)
	return nil
}
func (q *partialQueue) Release(context.Context, string, string) error { return nil }
func (q *partialQueue) Reap(context.Context, time.Duration) ([]store.ReapedRow, error) {
	return nil, nil
}

// Rows an earlier probe already claimed must be processed even though a later
// probe failed; otherwise they sit in processing until the lease expires.
func TestClaimErrorStillProcessesRowsAlreadyClaimed(t *testing.T) {
	q := &partialQueue{}
	var sends atomic.Int32
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		sends.Add(1)
		return okSender(ctx, m)
	})
	start(t, worker.New(q, sender, okLimiter, testSettings(), worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	waitFor(t, "row from the failed claim was sent", func() bool { return q.finished.Load() == 1 })
	if sends.Load() != 1 {
		t.Fatalf("sends = %d, want 1", sends.Load())
	}
}
