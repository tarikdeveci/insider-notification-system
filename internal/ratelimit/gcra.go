// Package ratelimit implements a distributed GCRA rate limiter on Redis.
package ratelimit

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed gcra.lua
var gcraSource string

var gcraScript = redis.NewScript(gcraSource)

// ErrWaitExceeded is returned when admission would need more than the max wait.
var ErrWaitExceeded = errors.New("ratelimit: wait exceeds max wait")

const (
	defaultInterval = 10 * time.Millisecond
	defaultMaxWait  = 10 * time.Second
	ttlMargin       = 100 * time.Millisecond
	keyPrefix       = "ratelimit:"
)

// Limiter admits at most one permit per interval per channel across replicas.
type Limiter struct {
	rdb      redis.UniversalClient
	interval time.Duration
	maxWait  time.Duration
	clock    func() time.Time
	sleep    func(context.Context, time.Duration) error
	hook     func(channel string, slot time.Time)
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithInterval sets the emission interval (default 10ms).
func WithInterval(d time.Duration) Option { return func(l *Limiter) { l.interval = d } }

// WithMaxWait sets the overall wait budget of one Acquire (default 10s).
func WithMaxWait(d time.Duration) Option { return func(l *Limiter) { l.maxWait = d } }

// WithClock sets the local clock used only to time the wait budget.
func WithClock(f func() time.Time) Option { return func(l *Limiter) { l.clock = f } }

// WithSleep sets the local sleeper used only between retries.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(l *Limiter) { l.sleep = f }
}

// WithGrantHook registers a callback invoked once per granted permit with the Redis slot time.
func WithGrantHook(f func(channel string, slot time.Time)) Option {
	return func(l *Limiter) { l.hook = f }
}

// New builds a Limiter on the given Redis client. The client must have
// ContextTimeoutEnabled set, otherwise the max wait bound does not cover stalled Redis I/O.
func New(rdb redis.UniversalClient, opts ...Option) *Limiter {
	l := &Limiter{
		rdb:      rdb,
		interval: defaultInterval,
		maxWait:  defaultMaxWait,
		clock:    time.Now,
		sleep:    sleepCtx,
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Acquire blocks until a permit for channel is granted and returns the total
// time waited. It fails closed: any error means no permit was granted.
func (l *Limiter) Acquire(ctx context.Context, channel string) (time.Duration, error) {
	start := l.clock()
	ttl := (l.interval + ttlMargin).Milliseconds()
	for {
		remaining := l.maxWait - l.clock().Sub(start)
		if remaining <= 0 {
			return l.clock().Sub(start), ErrWaitExceeded
		}
		callCtx, cancel := context.WithTimeout(ctx, remaining)
		res, err := gcraScript.Run(callCtx, l.rdb, []string{keyPrefix + channel},
			l.interval.Microseconds(), ttl).Int64Slice()
		cancel()
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return l.clock().Sub(start), ErrWaitExceeded
			}
			return l.clock().Sub(start), fmt.Errorf("ratelimit: acquire %s: %w", channel, err)
		}
		if len(res) != 2 {
			return l.clock().Sub(start), fmt.Errorf("ratelimit: acquire %s: unexpected script reply %v", channel, res)
		}
		waited := l.clock().Sub(start)
		if res[0] == 1 {
			if waited > l.maxWait {
				// Overdue grant: the Redis slot is consumed but the caller must not send (fail closed).
				return waited, ErrWaitExceeded
			}
			if l.hook != nil {
				l.hook(channel, time.UnixMicro(res[1]))
			}
			return waited, nil
		}
		retry := time.Duration(res[1]) * time.Microsecond
		if waited+retry > l.maxWait {
			return waited, ErrWaitExceeded
		}
		if err := l.sleep(ctx, retry); err != nil {
			return l.clock().Sub(start), err
		}
	}
}
