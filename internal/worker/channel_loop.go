package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/observability/tracectx"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/ratelimit"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

const (
	releaseTimeout = 5 * time.Second
	maxLastError   = 512
)

// Release reasons logged when a claimed row goes back unsent.
const (
	releaseWaitExceeded = "ratelimit_wait_exceeded"
	releaseLimiterError = "ratelimit_error"
	releaseShutdown     = "shutdown"
	releaseBreakerOpen  = "breaker_open"
)

// nextIdle doubles the idle wait up to max.
func nextIdle(cur, max time.Duration) time.Duration {
	if cur >= max/2 {
		return max
	}
	return cur * 2
}

// channelLoop claims and delivers rows of one channel until ctx is cancelled,
// then waits for its in-flight rows. Concurrency is a fixed set of sender
// slots: a Claim never takes more rows than there are free slots (at most
// ClaimBatch), so a claimed row starts at once. Each claimed row runs in its
// own goroutine so the permit waits overlap and stay inside the claim lease.
func (e *Engine) channelLoop(ctx, drain context.Context, ch domain.Channel) {
	log := e.log.With("channel", string(ch))
	slots := make(chan struct{}, e.set.Concurrency)
	limit := min(e.set.ClaimBatch, e.set.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()

	brk := e.brk[ch]
	idle := e.set.IdleMin
	for {
		if !e.pauseWhileOpen(ctx, brk) {
			return
		}
		n, ok := takeSlots(ctx, slots, limit)
		if !ok {
			return
		}
		// Waiting for a slot can outlast a breaker opening: decide again with slots in hand.
		mode, wait := brk.gate()
		if mode == gateNone {
			for i := 0; i < n; i++ {
				<-slots
			}
			if !sleep(ctx, e.breakerPause(wait)) {
				return
			}
			continue
		}
		if mode == gateOne {
			for ; n > 1; n-- {
				<-slots
			}
		}
		rows, err := e.claim(ctx, ch, n)
		for i := len(rows); i < n; i++ {
			<-slots // free the slots no row will use
		}
		if err != nil && ctx.Err() == nil {
			log.Error("claim failed", observability.ErrAttrs(err)...)
		}
		// A failed claim may still hand back rows an earlier probe moved to
		// processing: they are ours and are processed below.
		if len(rows) == 0 {
			if !sleep(ctx, idle) {
				return
			}
			idle = nextIdle(idle, e.set.IdleMax)
			continue
		}
		idle = e.set.IdleMin
		for _, row := range rows {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				e.process(ctx, drain, log, row)
			}()
		}
	}
}

// takeSlots blocks until one slot is free, then takes more without blocking,
// up to limit. It reports false when ctx is done first.
func takeSlots(ctx context.Context, slots chan struct{}, limit int) (int, bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return 0, false
	}
	n := 1
	for n < limit {
		select {
		case slots <- struct{}{}:
			n++
		default:
			return n, true
		}
	}
	return n, true
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// process runs one claimed row: permit, StartAttempt, send, result. ctx is the
// run context (cancelled at shutdown) and bounds everything up to StartAttempt;
// drain outlives it by ShutdownTimeout and carries Send and the result write of
// an attempt that has started.
func (e *Engine) process(ctx, drain context.Context, log *slog.Logger, row store.QueueRow) {
	ch := string(row.Channel)
	// The deliver span continues the trace the API stored on the row; an empty or
	// malformed traceparent just starts a new trace. drain carries the same span
	// so the send and the result write stay inside it.
	ctx, span := e.tracer.Start(tracectx.WithTraceParent(ctx, row.TraceParent), "deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("notification.id", row.ID),
			attribute.String("notification.channel", ch),
			attribute.Int("notification.priority", int(row.Priority)),
		))
	defer span.End()
	drain = trace.ContextWithSpan(drain, span)
	log = log.With("notification_id", row.ID, "correlation_id", row.CorrelationID)
	if row.BatchID != nil {
		log = log.With("batch_id", *row.BatchID)
	}

	brk := e.brk[row.Channel]
	if mode, _ := brk.gate(); mode == gateNone {
		// Claimed just before the breaker opened: do not wait for a permit it will not use.
		e.release(ctx, log, row, releaseBreakerOpen)
		return
	}

	waited, err := e.lim.Acquire(ctx, ch)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err() // admitted just as shutdown began: do not start a send
	}
	if err != nil {
		reason := releaseLimiterError
		switch {
		case ctx.Err() != nil:
			reason = releaseShutdown
		case errors.Is(err, ratelimit.ErrWaitExceeded):
			reason = releaseWaitExceeded
		}
		// A wait that hit its bound is a real observation of how long rows wait.
		// A limiter error carries no wait (waited is 0) and shutdown cuts the
		// wait short, so neither may skew the histogram toward zero.
		if reason == releaseWaitExceeded {
			e.obs.RateLimitWait(ch, waited)
		}
		e.release(ctx, log, row, reason)
		return
	}
	e.obs.RateLimitWait(ch, waited)

	tk, admitted := brk.admit()
	if !admitted {
		e.release(ctx, log, row, releaseBreakerOpen)
		return
	}
	defer tk.abort() // a no-op once the send is recorded; frees a probe slot on every other exit

	// StartAttempt runs on the run ctx: once shutdown begins no new attempt may start.
	attempt, startedAt, err := e.q.StartAttempt(ctx, row.ID, row.LeaseToken)
	if errors.Is(err, store.ErrLeaseLost) {
		log.Debug("lease lost before attempt start, dropping row")
		return
	}
	if err != nil && ctx.Err() != nil {
		// Unknown whether the attempt was counted: the reaper takes the row after the lease expires.
		log.Info("attempt not started", "reason", releaseShutdown)
		return
	}
	if err != nil {
		// Unknown whether the attempt was counted: leave the row to the reaper.
		log.Error("start attempt failed", observability.ErrAttrs(err)...)
		return
	}
	log = log.With("attempt", attempt)
	span.SetAttributes(attribute.Int("notification.attempt", attempt))

	began := e.now()
	sendCtx, send := e.tracer.Start(drain, "provider.send", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("notification.channel", ch)))
	res := e.sender.Send(sendCtx, provider.Message{ID: row.ID, To: row.Recipient, Channel: ch, Content: row.Content})
	send.SetAttributes(attribute.String("provider.result", res.Class.String()))
	if res.HTTPStatus != 0 {
		send.SetAttributes(attribute.Int("http.response.status_code", res.HTTPStatus))
	}
	if res.ErrCode != "" {
		send.SetAttributes(attribute.String("error.type", res.ErrCode))
	}
	if res.Class != provider.Success {
		send.SetStatus(codes.Error, res.Class.String())
		span.SetStatus(codes.Error, res.Class.String())
	}
	send.End()
	tk.record(res.Class)
	e.obs.Attempt(ch, res.Class, e.now().Sub(began))

	out := e.set.Policy.Decide(Input{
		Result: res, Attempts: attempt, MaxAttempts: row.MaxAttempts,
		CreatedAt: row.CreatedAt, ScheduledAt: row.ScheduledAt, Now: e.now(),
	}, e.rnd)

	fp := store.FinishParams{
		ID: row.ID, Token: row.LeaseToken, AttemptNo: attempt, StartedAt: startedAt,
		HTTPStatus: res.HTTPStatus, AttemptError: res.LastError(),
	}
	switch out.Action {
	case ActionSent:
		fp.Action, fp.ProviderMessageID = store.FinishSent, res.MessageID
	case ActionRetry:
		fp.Action, fp.NextAttemptAt, fp.GiveBackAttempt = store.FinishRetry, out.NextAttemptAt, out.GiveBackAttempt
		fp.LastError = res.LastError()
	default:
		fp.Action = store.FinishFail
		fp.LastError = truncateRunes(out.Reason+": "+res.LastError(), maxLastError)
	}

	err = e.q.FinishAttempt(drain, fp)
	if errors.Is(err, store.ErrLeaseLost) {
		log.Warn("lease lost before result write, dropping result", "result", res.Class.String())
		return
	}
	if err != nil {
		// The row stays processing until the lease expires and the reaper takes it.
		log.Error("finish attempt failed", append(observability.ErrAttrs(err), "result", res.Class.String())...)
		return
	}

	log.Info("attempt finished", "result", res.Class.String(), "error_code", res.ErrCode, "http_status", res.HTTPStatus)
	switch out.Action {
	case ActionSent:
		e.obs.Sent(ch, e.now().Sub(row.CreatedAt))
	case ActionFail:
		e.obs.Failed(ch, out.Reason)
	}
}

// release returns an unstarted row to pending. It runs on a context detached
// from ctx's cancellation (shutdown must not kill it) with a short timeout.
func (e *Engine) release(ctx context.Context, log *slog.Logger, row store.QueueRow, reason string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	err := e.q.Release(rctx, row.ID, row.LeaseToken)
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		log.Debug("lease lost before release, dropping row", "reason", reason)
	case err != nil:
		log.Error("release failed", append(observability.ErrAttrs(err), "reason", reason)...)
	case reason == releaseLimiterError:
		log.Warn("row released", "reason", reason)
	default:
		log.Info("row released", "reason", reason)
	}
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// breakerPause is how long a closed gate sleeps before the next look: the
// time left of the open period, bounded by IdleMax (zero wait means a probe is in flight).
func (e *Engine) breakerPause(wait time.Duration) time.Duration {
	if wait > 0 {
		return min(wait, e.set.IdleMax)
	}
	return e.set.IdleMax
}

// pauseWhileOpen sleeps while the breaker forbids claiming and reports false
// when ctx ended.
func (e *Engine) pauseWhileOpen(ctx context.Context, brk *breaker) bool {
	for {
		mode, wait := brk.gate()
		if mode != gateNone {
			return ctx.Err() == nil
		}
		if !sleep(ctx, e.breakerPause(wait)) {
			return false
		}
	}
}

// claim runs one Claim call. A claim serves many notifications, so it is its
// own root span. Idle polls (no rows, no error) record nothing: the span is
// started after the call with the call's start time, so a busy claim keeps its
// real duration.
func (e *Engine) claim(ctx context.Context, ch domain.Channel, n int) ([]store.QueueRow, error) {
	began := e.now()
	rows, err := e.q.Claim(ctx, ch, n, newToken())
	if len(rows) == 0 && (err == nil || ctx.Err() != nil) { // idle poll, or cut short by shutdown
		return rows, err
	}
	_, span := e.tracer.Start(ctx, "claim", trace.WithNewRoot(), trace.WithTimestamp(began),
		trace.WithAttributes(attribute.String("notification.channel", string(ch)), attribute.Int("claim.rows", len(rows))))
	if err != nil {
		span.SetStatus(codes.Error, "claim failed")
	}
	span.End()
	return rows, err
}
