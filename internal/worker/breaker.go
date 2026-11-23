package worker

import (
	"sync"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/provider"
)

// Breaker states, also the values of the circuit_breaker_state gauge.
const (
	breakerClosed   = 0
	breakerOpen     = 1
	breakerHalfOpen = 2
)

// gateMode tells the claim loop what the breaker allows.
type gateMode int

const (
	gateAll  gateMode = iota // claim as usual
	gateOne                  // claim at most one row (probe)
	gateNone                 // do not claim
)

// breaker is a per channel circuit breaker, in process only. Only Retryable
// provider results count as failures; Success and Permanent mean the provider
// answered and reset the count; Throttled neither counts nor resets.
//
// Work is admitted through admit, which returns a ticket. The ticket ends in
// record (a send happened) or abort (it did not); both are idempotent, so a
// deferred abort after record cannot leak a half open probe slot.
type breaker struct {
	threshold int
	openFor   time.Duration
	now       func() time.Time
	onChange  func(state, failures int)

	mu      sync.Mutex
	state   int
	fails   int
	opened  time.Time
	probing bool
}

func newBreaker(threshold int, openFor time.Duration, now func() time.Time, onChange func(state, failures int)) *breaker {
	return &breaker{threshold: threshold, openFor: openFor, now: now, onChange: onChange}
}

// gate is the non-mutating claim check. wait is how long until the open
// period ends (zero when unknown or not applicable).
func (b *breaker) gate() (mode gateMode, wait time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerOpen:
		if left := b.opened.Add(b.openFor).Sub(b.now()); left > 0 {
			return gateNone, left
		}
		return gateOne, 0
	case breakerHalfOpen:
		if b.probing {
			return gateNone, 0
		}
		return gateOne, 0
	}
	return gateAll, 0
}

// ticket is one admission. Use record after a send, abort otherwise.
type ticket struct {
	b     *breaker
	probe bool
	done  bool // guarded by b.mu
}

// admit decides whether one row may start. It mutates: the first admission
// after the open period moves the breaker to half open and becomes the probe.
func (b *breaker) admit() (*ticket, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerOpen:
		if b.now().Before(b.opened.Add(b.openFor)) {
			return nil, false
		}
		b.setState(breakerHalfOpen)
		b.probing = true
		return &ticket{b: b, probe: true}, true
	case breakerHalfOpen:
		if b.probing {
			return nil, false
		}
		b.probing = true
		return &ticket{b: b, probe: true}, true
	}
	return &ticket{b: b}, true
}

// record reports the provider result of the send this ticket admitted.
func (t *ticket) record(c provider.Class) {
	b := t.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	if t.probe {
		b.probing = false
		switch c {
		case provider.Success, provider.Permanent:
			b.fails = 0
			b.setState(breakerClosed)
		case provider.Retryable:
			b.opened = b.now()
			b.setState(breakerOpen)
		}
		return // Throttled: slot freed, still half open
	}
	if b.state != breakerClosed {
		return // admitted before the breaker opened: its result does not move the state
	}
	switch c {
	case provider.Success, provider.Permanent:
		b.fails = 0
	case provider.Retryable:
		b.fails++
		if b.fails >= b.threshold {
			b.opened = b.now()
			b.setState(breakerOpen)
		}
	}
}

// abort ends the ticket without a send, freeing a probe slot. Safe to call
// after record and more than once.
func (t *ticket) abort() {
	b := t.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	if t.probe {
		b.probing = false
	}
}

// setState must run with b.mu held.
func (b *breaker) setState(s int) {
	b.state = s
	if b.onChange != nil {
		b.onChange(s, b.fails)
	}
}
