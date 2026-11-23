package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

const retryMissingID = "123e4567-e89b-12d3-a456-426614174000"

// failedRow creates an sms row and forces it into the failed state the way the
// worker leaves an exhausted notification.
func failedRow(t *testing.T, s *store.Store, pool *pgxpool.Pool) domain.Notification {
	t.Helper()
	n := mustCreate(t, s, domain.ChannelSMS, "dead")
	exec(t, pool, `UPDATE notifications SET status='failed', attempts=8, last_error='provider 503',
		next_attempt_at = now() + interval '1 hour', lease_token = gen_random_uuid(), locked_until = now() + interval '1 minute'
		WHERE id=$1::uuid`, n.ID)
	return n
}

func TestRetryRequeuesFailed(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	n := failedRow(t, s, pool)

	got, err := s.RetryNotification(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusPending || got.Attempts != 0 {
		t.Fatalf("status=%s attempts=%d", got.Status, got.Attempts)
	}
	if got.LastError == nil || *got.LastError != "provider 503" {
		t.Fatalf("last_error must be kept: %v", got.LastError)
	}
	var diff float64
	var leaseNull, lockedNull bool
	if err := pool.QueryRow(ctx,
		`SELECT abs(extract(epoch FROM (now() - next_attempt_at))), lease_token IS NULL, locked_until IS NULL
		 FROM notifications WHERE id=$1::uuid`, n.ID).Scan(&diff, &leaseNull, &lockedNull); err != nil {
		t.Fatal(err)
	}
	if diff > 1 || !leaseNull || !lockedNull {
		t.Fatalf("next_attempt_at diff=%v lease null=%v locked null=%v", diff, leaseNull, lockedNull)
	}
	rows, err := s.Claim(ctx, domain.ChannelSMS, 5, newToken(t, pool))
	if err != nil || len(rows) != 1 || rows[0].ID != n.ID {
		t.Fatalf("claim after retry: %v %d", err, len(rows))
	}
}

func TestRetryNotFailedIs409(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	for _, st := range []string{"pending", "processing", "sent", "cancelled"} {
		n := mustCreate(t, s, domain.ChannelSMS, "x")
		exec(t, pool, `UPDATE notifications SET status=$2 WHERE id=$1::uuid`, n.ID, st)
		if _, err := s.RetryNotification(ctx, n.ID); !errors.Is(err, store.ErrNotFailed) {
			t.Fatalf("%s: want ErrNotFailed, got %v", st, err)
		}
	}
}

func TestRetryMissingIs404(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.RetryNotification(context.Background(), retryMissingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRetryRacesReaperAndCancel(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	t.Run("concurrent retries exactly one wins", func(t *testing.T) {
		n := failedRow(t, s, pool)
		var ok, notFailed atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.RetryNotification(ctx, n.ID)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, store.ErrNotFailed):
					notFailed.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 1 || notFailed.Load() != 19 {
			t.Fatalf("ok=%d notFailed=%d", ok.Load(), notFailed.Load())
		}
	})

	t.Run("cancel after retry once then retry is 409", func(t *testing.T) {
		n := failedRow(t, s, pool)
		if _, err := s.RetryNotification(ctx, n.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CancelNotification(ctx, n.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := s.CancelNotification(ctx, n.ID); !errors.Is(err, store.ErrNotPending) {
			t.Fatalf("second cancel: %v", err)
		}
		if _, err := s.RetryNotification(ctx, n.ID); !errors.Is(err, store.ErrNotFailed) {
			t.Fatalf("retry cancelled: %v", err)
		}
	})

	t.Run("reaper never reverts a retried row", func(t *testing.T) {
		n := failedRow(t, s, pool)
		var wg sync.WaitGroup
		wg.Add(2)
		var retryErr, reapErr error
		go func() { defer wg.Done(); _, retryErr = s.RetryNotification(ctx, n.ID) }()
		go func() { defer wg.Done(); _, reapErr = s.Reap(ctx, time.Hour) }()
		wg.Wait()
		if retryErr != nil || reapErr != nil {
			t.Fatalf("retry=%v reap=%v", retryErr, reapErr)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM notifications WHERE id=$1::uuid`, n.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "pending" {
			t.Fatalf("status %s", status)
		}
	})
}
