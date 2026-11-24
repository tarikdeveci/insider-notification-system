// Package worker turns provider results into queue decisions and runs the
// delivery loop. This file is the pure retry policy: no I/O, no clock, no
// global randomness.
package worker

import (
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/provider"
)

// Failure reasons: the worker prefixes notifications.last_error with them
// ("<reason>: <error>") and they are the `reason` label of
// notifications_failed_total.
const (
	ReasonPermanent = "permanent"
	ReasonExhausted = "exhausted"
	ReasonExpired   = "expired"
)

// Policy holds the retry timing knobs.
type Policy struct {
	Base, Cap, MaxAge time.Duration
}

// DefaultPolicy returns the spec defaults: base 2s, cap 10m, max age 24h.
func DefaultPolicy() Policy {
	return Policy{Base: 2 * time.Second, Cap: 10 * time.Minute, MaxAge: 24 * time.Hour}
}

// Backoff is full jitter: rnd() * min(cap, base*2^(attempt-1)), with rnd in
// [0,1). Attempts below 1 count as 1. The doubling stops at the cap so large
// attempt numbers cannot overflow.
func Backoff(attempt int, base, cap time.Duration, rnd func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ceil := base
	for i := 1; i < attempt && ceil < cap; i++ {
		if ceil > cap/2 {
			ceil = cap
			break
		}
		ceil *= 2
	}
	if ceil > cap {
		ceil = cap
	}
	if ceil <= 0 {
		return 0
	}
	return time.Duration(rnd() * float64(ceil))
}

// Backoff applies the policy's base and cap.
func (p Policy) Backoff(attempt int, rnd func() float64) time.Duration {
	return Backoff(attempt, p.Base, p.Cap, rnd)
}

// Input is everything Decide needs. Attempts is the row's attempts value after
// this attempt's increment, so the first send has Attempts 1.
type Input struct {
	Result      provider.Result
	Attempts    int
	MaxAttempts int
	CreatedAt   time.Time
	ScheduledAt *time.Time
	RequeuedAt  *time.Time // a manual retry restarts the age window
	Now         time.Time
}

// Action is what the worker does with the row next.
type Action int

const (
	ActionSent Action = iota
	ActionRetry
	ActionFail
)

// Outcome is the decision. Delay is set for ActionRetry and the store adds it
// to the database clock, Reason is set for ActionFail, and GiveBackAttempt asks
// the store to refund the attempt.
type Outcome struct {
	Action          Action
	Delay           time.Duration
	GiveBackAttempt bool
	Reason          string
}

// Decide maps one provider result to an Outcome. It reads no clock: Now and
// the jitter source are injected.
func (p Policy) Decide(in Input, rnd func() float64) Outcome {
	switch in.Result.Class {
	case provider.Success:
		return Outcome{Action: ActionSent}
	case provider.Permanent:
		return Outcome{Action: ActionFail, Reason: ReasonPermanent}
	}
	origin := in.CreatedAt
	if in.ScheduledAt != nil {
		origin = *in.ScheduledAt
	}
	if in.RequeuedAt != nil && in.RequeuedAt.After(origin) {
		origin = *in.RequeuedAt
	}
	if !in.Now.Before(origin.Add(p.MaxAge)) {
		return Outcome{Action: ActionFail, Reason: ReasonExpired}
	}
	if in.Result.Class == provider.Throttled {
		delay := in.Result.RetryAfter
		if delay <= 0 {
			delay = p.Backoff(in.Attempts, rnd)
		}
		return Outcome{Action: ActionRetry, Delay: delay, GiveBackAttempt: true}
	}
	if in.Attempts >= in.MaxAttempts {
		return Outcome{Action: ActionFail, Reason: ReasonExhausted}
	}
	return Outcome{Action: ActionRetry, Delay: p.Backoff(in.Attempts, rnd)}
}
