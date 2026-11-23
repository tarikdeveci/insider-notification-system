package worker

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

// Queue is the slice of *store.Store the engine uses.
type Queue interface {
	Claim(ctx context.Context, ch domain.Channel, n int, token string) ([]store.QueueRow, error)
	StartAttempt(ctx context.Context, id, token string) (attempt int, startedAt time.Time, err error)
	FinishAttempt(ctx context.Context, p store.FinishParams) error
	Release(ctx context.Context, id, token string) error
	Reap(ctx context.Context, maxAge time.Duration) ([]store.ReapedRow, error)
}

// Sender delivers one message; *provider.Client satisfies it.
type Sender interface {
	Send(ctx context.Context, m provider.Message) provider.Result
}

// Limiter admits sends; *ratelimit.Limiter satisfies it.
type Limiter interface {
	Acquire(ctx context.Context, channel string) (time.Duration, error)
}

// Observer receives delivery events. Implementations must be safe for
// concurrent use and must not block.
type Observer interface {
	Attempt(channel string, result provider.Class, d time.Duration)
	Sent(channel string, endToEnd time.Duration)
	Failed(channel, reason string)
	RateLimitWait(channel string, d time.Duration)
	// BreakerState reports a circuit breaker change: 0 closed, 1 open, 2 half open.
	BreakerState(channel string, state int)
}

// NopObserver ignores every event.
type NopObserver struct{}

func (NopObserver) Attempt(string, provider.Class, time.Duration) {}
func (NopObserver) Sent(string, time.Duration)                    {}
func (NopObserver) Failed(string, string)                         {}
func (NopObserver) RateLimitWait(string, time.Duration)           {}
func (NopObserver) BreakerState(string, int)                      {}

// Settings tunes the engine. Zero values fall back to the spec defaults.
type Settings struct {
	Concurrency      int // sender slots per channel
	ClaimBatch       int // most rows one Claim may take
	ShutdownTimeout  time.Duration
	ReaperInterval   time.Duration
	MaxDeliveryAge   time.Duration
	IdleMin          time.Duration
	IdleMax          time.Duration
	Policy           Policy
	BreakerThreshold int           // consecutive Retryable results that open the breaker
	BreakerOpenFor   time.Duration // how long an open breaker stops claiming
}

// DefaultSettings returns the spec defaults.
func DefaultSettings() Settings {
	return Settings{
		Concurrency:      10,
		ClaimBatch:       20,
		ShutdownTimeout:  15 * time.Second,
		ReaperInterval:   5 * time.Second,
		MaxDeliveryAge:   24 * time.Hour,
		IdleMin:          200 * time.Millisecond,
		IdleMax:          time.Second,
		Policy:           DefaultPolicy(),
		BreakerThreshold: 10,
		BreakerOpenFor:   30 * time.Second,
	}
}

func (s Settings) withDefaults() Settings {
	d := DefaultSettings()
	if s.Concurrency < 1 {
		s.Concurrency = d.Concurrency
	}
	if s.ClaimBatch < 1 {
		s.ClaimBatch = d.ClaimBatch
	}
	if s.ShutdownTimeout <= 0 {
		s.ShutdownTimeout = d.ShutdownTimeout
	}
	if s.ReaperInterval <= 0 {
		s.ReaperInterval = d.ReaperInterval
	}
	if s.MaxDeliveryAge <= 0 {
		s.MaxDeliveryAge = d.MaxDeliveryAge
	}
	if s.BreakerThreshold < 1 {
		s.BreakerThreshold = d.BreakerThreshold
	}
	if s.BreakerOpenFor <= 0 {
		s.BreakerOpenFor = d.BreakerOpenFor
	}
	if s.IdleMin <= 0 {
		s.IdleMin = d.IdleMin
	}
	if s.IdleMax <= 0 {
		s.IdleMax = d.IdleMax
	}
	if s.IdleMax < s.IdleMin {
		s.IdleMax = s.IdleMin
	}
	if s.Policy.Base <= 0 || s.Policy.Cap <= 0 {
		s.Policy.Base, s.Policy.Cap = d.Policy.Base, d.Policy.Cap
	}
	if s.Policy.MaxAge <= 0 {
		s.Policy.MaxAge = s.MaxDeliveryAge
	}
	return s
}

// Engine runs one claim loop per channel plus the reaper.
type Engine struct {
	q        Queue
	sender   Sender
	lim      Limiter
	set      Settings
	log      *slog.Logger
	obs      Observer
	rnd      func() float64
	now      func() time.Time
	channels []domain.Channel
	brk      map[domain.Channel]*breaker
}

// Option customizes an Engine.
type Option func(*Engine)

// WithLogger sets the logger (default discards output).
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// WithObserver sets the delivery observer (default NopObserver).
func WithObserver(o Observer) Option {
	return func(e *Engine) {
		if o != nil {
			e.obs = o
		}
	}
}

// WithRand sets the jitter source, a function returning values in [0,1).
func WithRand(f func() float64) Option {
	return func(e *Engine) {
		if f != nil {
			e.rnd = f
		}
	}
}

// WithClock sets the clock used for retry decisions and latency.
func WithClock(f func() time.Time) Option {
	return func(e *Engine) {
		if f != nil {
			e.now = f
		}
	}
}

// WithChannels restricts the engine to the given channels (default sms, email, push).
func WithChannels(chs ...domain.Channel) Option {
	return func(e *Engine) {
		if len(chs) > 0 {
			e.channels = chs
		}
	}
}

// New builds an Engine.
func New(q Queue, s Sender, l Limiter, set Settings, opts ...Option) *Engine {
	e := &Engine{
		q: q, sender: s, lim: l,
		set:      set.withDefaults(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		obs:      NopObserver{},
		rnd:      rand.Float64,
		now:      time.Now,
		channels: []domain.Channel{domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush},
	}
	for _, o := range opts {
		o(e)
	}
	e.brk = make(map[domain.Channel]*breaker, len(e.channels))
	for _, ch := range e.channels {
		e.brk[ch] = newBreaker(e.set.BreakerThreshold, e.set.BreakerOpenFor, e.now, e.breakerChanged(ch))
	}
	return e
}

// Run starts the channel loops and the reaper and blocks until ctx is
// cancelled and draining is done. Cancelling ctx stops claiming and releases
// rows that have not started sending. Sends already started keep running on a
// drain context that is cancelled ShutdownTimeout after ctx.
func (e *Engine) Run(ctx context.Context) error {
	drain, cancelDrain := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelDrain()
	context.AfterFunc(ctx, func() { time.AfterFunc(e.set.ShutdownTimeout, cancelDrain) })

	var wg sync.WaitGroup
	for _, ch := range e.channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.channelLoop(ctx, drain, ch)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.reaperLoop(ctx)
	}()
	wg.Wait()
	return nil
}

func (e *Engine) reaperLoop(ctx context.Context) {
	t := time.NewTicker(e.set.ReaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rows, err := e.q.Reap(ctx, e.set.MaxDeliveryAge)
		if err != nil {
			if ctx.Err() == nil {
				e.log.Error("reaper failed", observability.ErrAttrs(err)...)
			}
			continue
		}
		requeued, failed := 0, 0
		for _, r := range rows {
			if r.Status != domain.StatusFailed {
				requeued++
				continue
			}
			failed++
			e.log.Warn("reaper failed row", "notification_id", r.ID, "channel", string(r.Channel), "reason", r.Reason)
			e.obs.Failed(string(r.Channel), r.Reason)
		}
		if requeued+failed > 0 {
			e.log.Info("reaper pass", "requeued", requeued, "failed", failed)
		}
	}
}

// newToken returns a random version 4 UUID string (no uuid library).
func newToken() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic("worker: crypto/rand failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// breakerChanged returns the state change callback of one channel's breaker:
// it updates the gauge and logs the transition (channel and count only).
func (e *Engine) breakerChanged(ch domain.Channel) func(state, failures int) {
	name := string(ch)
	return func(state, failures int) {
		e.obs.BreakerState(name, state)
		switch state {
		case breakerOpen:
			e.log.Warn("circuit breaker opened", "channel", name, "failures", failures)
		case breakerHalfOpen:
			e.log.Info("circuit breaker half open", "channel", name, "failures", failures)
		default:
			e.log.Info("circuit breaker closed", "channel", name, "failures", failures)
		}
	}
}
