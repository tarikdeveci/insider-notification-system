package worker

import (
	"sync"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/provider"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type transition struct{ state, failures int }

func testBreaker() (*breaker, *fakeClock, *[]transition) {
	clk := newFakeClock()
	var seen []transition
	b := newBreaker(10, 30*time.Second, clk.now, func(s, f int) { seen = append(seen, transition{s, f}) })
	return b, clk, &seen
}

// send admits one row and records c, failing the test if it is not admitted.
func send(t *testing.T, b *breaker, c provider.Class) {
	t.Helper()
	tk, ok := b.admit()
	if !ok {
		t.Fatalf("admit refused in state %d", b.state)
	}
	tk.record(c)
}

func fail(t *testing.T, b *breaker, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		send(t, b, provider.Retryable)
	}
}

func TestOpensAfter10ConsecutiveFailures(t *testing.T) {
	b, _, seen := testBreaker()
	fail(t, b, 9)
	if b.state != breakerClosed || len(*seen) != 0 {
		t.Fatalf("after 9 failures state = %d, transitions %v; want closed and none", b.state, *seen)
	}
	if mode, _ := b.gate(); mode != gateAll {
		t.Fatalf("closed gate = %v, want gateAll", mode)
	}
	fail(t, b, 1)
	if b.state != breakerOpen {
		t.Fatalf("after 10 failures state = %d, want open", b.state)
	}
	if len(*seen) != 1 || (*seen)[0] != (transition{breakerOpen, 10}) {
		t.Fatalf("transitions = %v, want one open with 10 failures", *seen)
	}
	if _, ok := b.admit(); ok {
		t.Fatal("open breaker admitted work before the open period ended")
	}
}

func TestSuccessResetsCount(t *testing.T) {
	for _, reset := range []provider.Class{provider.Success, provider.Permanent} {
		b, _, _ := testBreaker()
		fail(t, b, 9)
		send(t, b, reset)
		fail(t, b, 9)
		if b.state != breakerClosed {
			t.Fatalf("%v: state = %d after 9, reset, 9; want closed", reset, b.state)
		}
		fail(t, b, 1)
		if b.state != breakerOpen {
			t.Fatalf("%v: state = %d, want open at the 10th failure after the reset", reset, b.state)
		}
	}
	b, _, _ := testBreaker()
	fail(t, b, 5)
	send(t, b, provider.Throttled)
	fail(t, b, 5)
	if b.state != breakerOpen {
		t.Fatalf("5 failures, Throttled, 5 failures: state = %d, want open (Throttled does not reset)", b.state)
	}
}

func TestThrottledDoesNotCount(t *testing.T) {
	b, _, _ := testBreaker()
	for i := 0; i < 30; i++ {
		send(t, b, provider.Throttled)
	}
	if b.state != breakerClosed {
		t.Fatalf("state = %d after 30 Throttled, want closed", b.state)
	}
}

// dueBreaker returns a breaker that opened and whose open period just ended.
func dueBreaker(t *testing.T) (*breaker, *fakeClock, *[]transition) {
	t.Helper()
	b, clk, seen := testBreaker()
	fail(t, b, 10)
	clk.advance(30 * time.Second)
	return b, clk, seen
}

func TestOpenGateReportsRemainingThenHalfOpenProbe(t *testing.T) {
	b, clk, _ := testBreaker()
	fail(t, b, 10)
	clk.advance(10 * time.Second)
	if mode, wait := b.gate(); mode != gateNone || wait != 20*time.Second {
		t.Fatalf("gate = %v, %v; want gateNone, 20s", mode, wait)
	}
	clk.advance(20 * time.Second)
	if mode, _ := b.gate(); mode != gateOne {
		t.Fatalf("gate at openedAt+30s = %v, want gateOne", mode)
	}
	if b.state != breakerOpen {
		t.Fatal("gate must not mutate: state should still be open until a probe is admitted")
	}
}

func TestHalfOpenSuccessCloses(t *testing.T) {
	for _, c := range []provider.Class{provider.Success, provider.Permanent} {
		b, _, seen := dueBreaker(t)
		tk, ok := b.admit()
		if !ok || b.state != breakerHalfOpen {
			t.Fatalf("probe admission ok=%v state=%d, want admitted and half open", ok, b.state)
		}
		tk.record(c)
		if b.state != breakerClosed || b.fails != 0 {
			t.Fatalf("%v: state=%d fails=%d, want closed and 0", c, b.state, b.fails)
		}
		want := []transition{{breakerOpen, 10}, {breakerHalfOpen, 10}, {breakerClosed, 0}}
		if len(*seen) != 3 || (*seen)[0] != want[0] || (*seen)[1] != want[1] || (*seen)[2] != want[2] {
			t.Fatalf("transitions = %v, want %v", *seen, want)
		}
		if mode, _ := b.gate(); mode != gateAll {
			t.Fatalf("closed gate = %v", mode)
		}
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b, clk, _ := dueBreaker(t)
	tk, _ := b.admit()
	clk.advance(5 * time.Second) // the probe takes 5s
	reopenedAt := clk.now()
	tk.record(provider.Retryable)
	if b.state != breakerOpen || !b.opened.Equal(reopenedAt) {
		t.Fatalf("state=%d openedAt=%v, want open with a fresh openedAt %v", b.state, b.opened, reopenedAt)
	}
	clk.advance(29 * time.Second)
	if _, ok := b.admit(); ok {
		t.Fatal("reopened breaker admitted before a full new open period")
	}
	clk.advance(time.Second)
	if _, ok := b.admit(); !ok {
		t.Fatal("reopened breaker refused after the new open period")
	}
}

func TestHalfOpenAllowsSingleProbe(t *testing.T) {
	b, _, _ := dueBreaker(t)
	const n = 50
	var admitted int
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := b.admit(); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted != 1 {
		t.Fatalf("admitted = %d concurrent probes, want exactly 1", admitted)
	}
	if mode, _ := b.gate(); mode != gateNone {
		t.Fatalf("gate with a probe in flight = %v, want gateNone", mode)
	}
}

func TestHalfOpenProbeAbortAllowsNextProbe(t *testing.T) {
	b, _, _ := dueBreaker(t)
	tk, _ := b.admit()
	tk.abort()
	tk.abort() // idempotent
	if b.state != breakerHalfOpen {
		t.Fatalf("state = %d after abort, want still half open", b.state)
	}
	if mode, _ := b.gate(); mode != gateOne {
		t.Fatalf("gate after abort = %v, want gateOne", mode)
	}
	next, ok := b.admit()
	if !ok {
		t.Fatal("next probe refused after abort")
	}
	tk.record(provider.Success) // a stale ticket must not touch the new probe
	if b.state != breakerHalfOpen || !b.probing {
		t.Fatalf("stale ticket moved the breaker: state=%d probing=%v", b.state, b.probing)
	}
	next.record(provider.Success)
	if b.state != breakerClosed {
		t.Fatalf("state = %d, want closed", b.state)
	}
}

func TestHalfOpenThrottledProbeFreesSlotAndStaysHalfOpen(t *testing.T) {
	b, _, _ := dueBreaker(t)
	tk, _ := b.admit()
	tk.record(provider.Throttled)
	if b.state != breakerHalfOpen || b.probing {
		t.Fatalf("state=%d probing=%v, want half open with the slot free", b.state, b.probing)
	}
	if _, ok := b.admit(); !ok {
		t.Fatal("next probe refused after a Throttled probe")
	}
}

func TestRecordThenAbortDoesNotLeakOrDoubleCount(t *testing.T) {
	b, _, _ := dueBreaker(t)
	tk, _ := b.admit()
	tk.record(provider.Retryable)
	tk.abort() // the deferred abort in process runs after record
	tk.record(provider.Success)
	if b.state != breakerOpen {
		t.Fatalf("state = %d, want open: only the first outcome of a ticket counts", b.state)
	}
}

func TestClosedTicketAbortCountsNothing(t *testing.T) {
	b, _, _ := testBreaker()
	for i := 0; i < 20; i++ {
		tk, _ := b.admit()
		tk.abort()
	}
	fail(t, b, 9)
	if b.state != breakerClosed {
		t.Fatalf("state = %d, aborted tickets must not count", b.state)
	}
}

func TestStaleClosedTicketDoesNotMoveOpenBreaker(t *testing.T) {
	b, _, _ := testBreaker()
	slow, _ := b.admit() // admitted while closed, still sending
	fail(t, b, 10)
	slow.record(provider.Success)
	if b.state != breakerOpen {
		t.Fatalf("state = %d, a late result of a pre-open send must not close the breaker", b.state)
	}
}
