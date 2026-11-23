package ratelimit_test

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tarikdeveci/insider-notification-system/internal/ratelimit"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

const interval = 10 * time.Millisecond

func newClient(t *testing.T) *redis.Client {
	t.Helper()
	opts, err := redis.ParseURL(testutil.RedisURL(t))
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// uniqueChannel keeps limiter keys from leaking between repeated runs against the shared test Redis.
func uniqueChannel(base string) string {
	return base + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

type slots struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func (s *slots) hook(channel string, slot time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]time.Time{}
	}
	s.m[channel] = append(s.m[channel], slot)
}

func (s *slots) sorted(channel string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]time.Time(nil), s.m[channel]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// hammer runs workers goroutines acquiring on channel until d elapses.
func hammer(t *testing.T, limiters []*ratelimit.Limiter, channel string, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var wg sync.WaitGroup
	for _, l := range limiters {
		wg.Add(1)
		go func(l *ratelimit.Limiter) {
			defer wg.Done()
			for ctx.Err() == nil {
				if _, err := l.Acquire(ctx, channel); err != nil && ctx.Err() == nil {
					t.Errorf("acquire: %v", err)
					return
				}
			}
		}(l)
	}
	wg.Wait()
}

func assertSpacing(t *testing.T, got []time.Time) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		if d := got[i].Sub(got[i-1]); d < interval {
			t.Fatalf("slots %d and %d only %v apart, want >= %v", i-1, i, d, interval)
		}
	}
}

func replicas(rdb *redis.Client, n int, extra ...ratelimit.Option) []*ratelimit.Limiter {
	out := make([]*ratelimit.Limiter, n)
	for i := range out {
		out[i] = ratelimit.New(rdb, extra...)
	}
	return out
}

func TestGCRASlotsAtLeast10msApart(t *testing.T) {
	rdb := newClient(t)
	var s slots
	hammer(t, replicas(rdb, 8, ratelimit.WithGrantHook(s.hook)), "spacing", 3*time.Second)
	got := s.sorted("spacing")
	if len(got) < 20 {
		t.Fatalf("only %d permits granted, limiter looks stuck", len(got))
	}
	assertSpacing(t, got)
}

func TestNoHalfOpenSecondHoldsMoreThan100(t *testing.T) {
	rdb := newClient(t)
	var s slots
	hammer(t, replicas(rdb, 8, ratelimit.WithGrantHook(s.hook)), "window", 3*time.Second)
	got := s.sorted("window")
	if len(got) < 20 {
		t.Fatalf("only %d permits granted, limiter looks stuck", len(got))
	}
	j := 0
	for i := range got {
		if j < i {
			j = i
		}
		for j < len(got) && got[j].Before(got[i].Add(time.Second)) {
			j++
		}
		if n := j - i; n > 100 {
			t.Fatalf("window [%v, +1s) holds %d admissions, want <= 100", got[i], n)
		}
	}
}

func TestChannelsAreIndependent(t *testing.T) {
	rdb := newClient(t)
	var s slots
	l := ratelimit.New(rdb, ratelimit.WithGrantHook(s.hook))
	ctx := context.Background()
	// The 1h interval key outlives the test, so the channel names are unique per run (-count > 1 safe).
	sms, email := uniqueChannel("ind-sms"), uniqueChannel("ind-email")
	// Push the sms channel far into the future with a large interval limiter.
	slow := ratelimit.New(rdb, ratelimit.WithInterval(time.Hour))
	if _, err := slow.Acquire(ctx, sms); err != nil {
		t.Fatalf("sms acquire: %v", err)
	}
	if _, err := slow.Acquire(ctx, sms); !errors.Is(err, ratelimit.ErrWaitExceeded) {
		t.Fatalf("sms should be saturated, got %v", err)
	}
	wait, err := l.Acquire(ctx, email)
	if err != nil {
		t.Fatalf("email acquire: %v", err)
	}
	if wait > time.Second {
		t.Fatalf("email waited %v behind sms load, want no queueing", wait)
	}
}

func TestUsesRedisTime(t *testing.T) {
	rdb := newClient(t)
	var s slots
	skew := func(d time.Duration) ratelimit.Option {
		return ratelimit.WithClock(func() time.Time { return time.Now().Add(d) })
	}
	ls := []*ratelimit.Limiter{
		ratelimit.New(rdb, ratelimit.WithGrantHook(s.hook), skew(5*time.Second)),
		ratelimit.New(rdb, ratelimit.WithGrantHook(s.hook), skew(-5*time.Second)),
	}
	hammer(t, ls, "skew", time.Second)
	got := s.sorted("skew")
	if len(got) < 5 {
		t.Fatalf("only %d permits granted", len(got))
	}
	assertSpacing(t, got)
	if d := time.Since(got[0]); d > 30*time.Second || d < -30*time.Second {
		t.Fatalf("slot time %v not in real time range", got[0])
	}
}

func TestWaitBoundedByMaxWait(t *testing.T) {
	rdb := newClient(t)
	l := ratelimit.New(rdb, ratelimit.WithInterval(time.Second), ratelimit.WithMaxWait(25*time.Millisecond))
	ctx := context.Background()
	// The first permit uses a generous budget so a cold connection cannot trip the 25ms bound.
	warm := ratelimit.New(rdb, ratelimit.WithInterval(time.Second))
	if _, err := warm.Acquire(ctx, "bounded"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	start := time.Now()
	_, err := l.Acquire(ctx, "bounded")
	if !errors.Is(err, ratelimit.ErrWaitExceeded) {
		t.Fatalf("err = %v, want ErrWaitExceeded", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("returned after %v, want near the 25ms bound", el)
	}
}

func TestAcquireWaitsForSlotAndReportsWait(t *testing.T) {
	rdb := newClient(t)
	l := ratelimit.New(rdb, ratelimit.WithInterval(100*time.Millisecond), ratelimit.WithMaxWait(5*time.Second))
	ctx := context.Background()
	if w, err := l.Acquire(ctx, "waits"); err != nil || w > time.Second {
		t.Fatalf("first acquire = %v, %v", w, err)
	}
	w, err := l.Acquire(ctx, "waits")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if w < 50*time.Millisecond || w > 2*time.Second {
		t.Fatalf("wait = %v, want roughly the 100ms interval", w)
	}
}

func TestContextCancelStopsWaiting(t *testing.T) {
	rdb := newClient(t)
	l := ratelimit.New(rdb, ratelimit.WithInterval(2*time.Second), ratelimit.WithMaxWait(time.Minute))
	if _, err := l.Acquire(context.Background(), "ctxcancel"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "ctxcancel"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRedisDownFailsClosed(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	called := false
	l := ratelimit.New(rdb, ratelimit.WithGrantHook(func(string, time.Time) { called = true }))
	_, err := l.Acquire(context.Background(), "down")
	if err == nil {
		t.Fatal("Acquire returned nil error with Redis down, want fail closed")
	}
	if errors.Is(err, ratelimit.ErrWaitExceeded) {
		t.Fatalf("err = %v, want an infrastructure error", err)
	}
	if called {
		t.Fatal("grant hook fired without a permit")
	}
}

func TestOversleepingSleeperCannotGrantPastMaxWait(t *testing.T) {
	rdb := newClient(t)
	grants := 0
	oversleep := func(ctx context.Context, d time.Duration) error {
		time.Sleep(800 * time.Millisecond) // wakes far after the 500ms budget
		return nil
	}
	// A 200ms interval leaves wide room for the second call to land inside the first permit's slot even on a slow
	// host, while the needed wait (under 200ms) still fits the 500ms budget, so the sleeper path really runs.
	l := ratelimit.New(rdb, ratelimit.WithInterval(200*time.Millisecond), ratelimit.WithMaxWait(500*time.Millisecond),
		ratelimit.WithSleep(oversleep), ratelimit.WithGrantHook(func(string, time.Time) { grants++ }))
	ch := uniqueChannel("oversleep")
	if _, err := l.Acquire(context.Background(), ch); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// The slot of the first permit is still within its interval, so this is denied, sleeps long, then must not grant.
	_, err := l.Acquire(context.Background(), ch)
	if !errors.Is(err, ratelimit.ErrWaitExceeded) {
		t.Fatalf("err = %v, want ErrWaitExceeded", err)
	}
	if grants != 1 {
		t.Fatalf("grants = %d, want 1 (overdue permit must not be reported)", grants)
	}
}
