package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/ratelimit"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

// ---- fakes and helpers ----

type funcSender func(ctx context.Context, m provider.Message) provider.Result

func (f funcSender) Send(ctx context.Context, m provider.Message) provider.Result { return f(ctx, m) }

type funcLimiter func(ctx context.Context, channel string) (time.Duration, error)

func (f funcLimiter) Acquire(ctx context.Context, ch string) (time.Duration, error) {
	return f(ctx, ch)
}

var okLimiter = funcLimiter(func(context.Context, string) (time.Duration, error) { return 0, nil })

func okSender(ctx context.Context, m provider.Message) provider.Result {
	return provider.Result{Class: provider.Success, MessageID: "msg-" + m.ID[:8], HTTPStatus: 202}
}

// recQueue records the n of every Claim call.
type recQueue struct {
	worker.Queue
	mu     sync.Mutex
	claims []int
}

func (q *recQueue) Claim(ctx context.Context, ch domain.Channel, n int, token string) ([]store.QueueRow, error) {
	q.mu.Lock()
	q.claims = append(q.claims, n)
	q.mu.Unlock()
	return q.Queue.Claim(ctx, ch, n, token)
}

func (q *recQueue) ns() []int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]int(nil), q.claims...)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type recObserver struct {
	mu       sync.Mutex
	attempts []string
	sent     int
	failed   []string
	waits    int
}

func (o *recObserver) Attempt(ch string, c provider.Class, d time.Duration) {
	o.mu.Lock()
	o.attempts = append(o.attempts, ch+":"+c.String())
	o.mu.Unlock()
}
func (o *recObserver) Sent(ch string, d time.Duration) { o.mu.Lock(); o.sent++; o.mu.Unlock() }
func (o *recObserver) Failed(ch, reason string) {
	o.mu.Lock()
	o.failed = append(o.failed, ch+":"+reason)
	o.mu.Unlock()
}
func (o *recObserver) RateLimitWait(ch string, d time.Duration) {
	o.mu.Lock()
	o.waits++
	o.mu.Unlock()
}

func testSettings() worker.Settings {
	s := worker.DefaultSettings()
	s.Concurrency, s.ClaimBatch = 4, 4
	s.ShutdownTimeout = 2 * time.Second
	s.ReaperInterval = 50 * time.Millisecond
	s.IdleMin, s.IdleMax = 5*time.Millisecond, 20*time.Millisecond
	s.Policy = worker.Policy{Base: 10 * time.Millisecond, Cap: 50 * time.Millisecond, MaxAge: 24 * time.Hour}
	return s
}

func zeroRand() float64 { return 0 }

type env struct {
	pool *pgxpool.Pool
	st   *store.Store
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testutil.MigratedPool(t)
	return env{pool: pool, st: store.New(pool)}
}

func (e env) create(t *testing.T, ch domain.Channel, corr string) string {
	t.Helper()
	rcpt := map[domain.Channel]string{domain.ChannelSMS: "+905551234567", domain.ChannelEmail: "a@example.com", domain.ChannelPush: "tok123"}[ch]
	res, err := e.st.CreateNotification(context.Background(), store.CreateParams{
		New:           domain.NewNotification{Channel: ch, Recipient: rcpt, Content: "secret hello body", Priority: domain.PriorityNormal},
		CorrelationID: corr,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Notification.ID
}

type rowInfo struct {
	status       string
	attempts     int
	leaseCleared bool
	lastErr      string
	msgID        string
}

func (e env) row(t *testing.T, id string) rowInfo {
	t.Helper()
	var r rowInfo
	err := e.pool.QueryRow(context.Background(),
		`SELECT status, attempts, lease_token IS NULL AND locked_until IS NULL, COALESCE(last_error, ''), COALESCE(provider_message_id, '')
		 FROM notifications WHERE id = $1::uuid`, id).Scan(&r.status, &r.attempts, &r.leaseCleared, &r.lastErr, &r.msgID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e env) count(t *testing.T, status string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE status = $1`, status).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type running struct {
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, eng *worker.Engine) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- eng.Run(ctx) }()
	t.Cleanup(func() { r.cancel(); r.wait(t) })
	return r
}

func (r *running) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	opt, err := redis.ParseURL(testutil.RedisURL(t))
	if err != nil {
		t.Fatal(err)
	}
	opt.ContextTimeoutEnabled = true
	c := redis.NewClient(opt)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.FlushAll(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeQueue is a Queue whose Claim is scripted; every other method succeeds.
type fakeQueue struct{ onClaim func() []store.QueueRow }

func (f *fakeQueue) Claim(context.Context, domain.Channel, int, string) ([]store.QueueRow, error) {
	return f.onClaim(), nil
}
func (f *fakeQueue) StartAttempt(context.Context, string, string) (int, time.Time, error) {
	return 1, time.Now(), nil
}
func (f *fakeQueue) FinishAttempt(context.Context, store.FinishParams) error { return nil }
func (f *fakeQueue) Release(context.Context, string, string) error           { return nil }
func (f *fakeQueue) Reap(context.Context, time.Duration) ([]store.ReapedRow, error) {
	return nil, nil
}

// ---- tests ----

func TestOneLoopPerChannelIsolation(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		if m.Channel == "email" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return okSender(ctx, m)
	})
	emailID := e.create(t, domain.ChannelEmail, "c-email")
	smsID := e.create(t, domain.ChannelSMS, "c-sms")
	eng := worker.New(e.st, sender, okLimiter, testSettings(), worker.WithRand(zeroRand))
	start(t, eng)
	<-entered
	waitFor(t, "sms sent while email hangs", func() bool { return e.row(t, smsID).status == "sent" })
	if st := e.row(t, emailID).status; st != "processing" {
		t.Fatalf("email row should still be in flight, got %s", st)
	}
	close(release)
	waitFor(t, "email sent after release", func() bool { return e.row(t, emailID).status == "sent" })
}

func TestClaimsOnlyFreeSlots(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var mu sync.Mutex
	inflight := 0
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		mu.Lock()
		inflight++
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
		return okSender(ctx, m)
	})
	for i := 0; i < 10; i++ {
		e.create(t, domain.ChannelSMS, "c")
	}
	q := &recQueue{Queue: e.st}
	s := testSettings()
	s.Concurrency, s.ClaimBatch = 2, 5
	start(t, worker.New(q, sender, okLimiter, s, worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	waitFor(t, "two sends in flight", func() bool { mu.Lock(); defer mu.Unlock(); return inflight == 2 })
	time.Sleep(300 * time.Millisecond) // many idle cycles pass; no slot is free so nothing may be claimed
	if n := e.count(t, "processing"); n != 2 {
		t.Fatalf("processing rows = %d, want exactly the 2 free slots", n)
	}
	for _, n := range q.ns() {
		if n < 1 || n > 2 {
			t.Fatalf("Claim n = %d, want within 1..2 (free slots, batch 5)", n)
		}
	}
	close(release)
	waitFor(t, "all sent", func() bool { return e.count(t, "sent") == 10 })
}

func TestClaimBatchCapsClaimSize(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 9; i++ {
		e.create(t, domain.ChannelSMS, "c")
	}
	q := &recQueue{Queue: e.st}
	s := testSettings()
	s.Concurrency, s.ClaimBatch = 10, 3
	start(t, worker.New(q, funcSender(okSender), okLimiter, s, worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	waitFor(t, "all sent", func() bool { return e.count(t, "sent") == 9 })
	for _, n := range q.ns() {
		if n > 3 {
			t.Fatalf("Claim n = %d exceeds CLAIM_BATCH 3", n)
		}
	}
}

func TestRateLimiterWaitBoundedThenRelease(t *testing.T) {
	e := newEnv(t)
	rdb := testRedis(t)
	// One permit per hour: the first row is admitted, the second can never be within 150ms.
	real := ratelimit.New(rdb, ratelimit.WithInterval(time.Hour), ratelimit.WithMaxWait(150*time.Millisecond))
	// Signal the third Acquire: the second row was released once and is waiting again,
	// so the cancel below cannot land inside a Claim call.
	var calls atomic.Int32
	thirdAcquire := make(chan struct{})
	lim := funcLimiter(func(ctx context.Context, ch string) (time.Duration, error) {
		if calls.Add(1) == 3 {
			close(thirdAcquire)
		}
		return real.Acquire(ctx, ch)
	})
	first := e.create(t, domain.ChannelSMS, "c1")
	second := e.create(t, domain.ChannelSMS, "c2")
	logs := &syncBuf{}
	obs := &recObserver{}
	r := start(t, worker.New(e.st, funcSender(okSender), lim, testSettings(),
		worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS),
		worker.WithLogger(observability.NewLogger(logs, "debug")), worker.WithObserver(obs)))
	waitFor(t, "one row sent", func() bool { return e.count(t, "sent") == 1 })
	waitFor(t, "release logged", func() bool { return strings.Contains(logs.String(), "ratelimit_wait_exceeded") })
	select {
	case <-thirdAcquire:
	case <-time.After(15 * time.Second):
		t.Fatal("second row was never retried after release")
	}
	r.cancel()
	r.wait(t)
	sentID, otherID := first, second
	if e.row(t, first).status != "sent" {
		sentID, otherID = second, first
	}
	if e.row(t, sentID).status != "sent" {
		t.Fatal("neither row was sent")
	}
	o := e.row(t, otherID)
	if o.status != "pending" || o.attempts != 0 || !o.leaseCleared {
		t.Fatalf("released row must be pending, attempts 0, lease cleared: %+v logs: %s", o, logs.String())
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.waits == 0 {
		t.Error("observer saw no rate limit wait")
	}
}

func TestGracefulShutdownReleasesUnstartedRows(t *testing.T) {
	e := newEnv(t)
	proceed := make(chan struct{})
	entered := make(chan struct{}, 4)
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		entered <- struct{}{}
		select {
		case <-proceed:
		case <-ctx.Done():
		}
		return okSender(ctx, m)
	})
	var once sync.Once
	lim := funcLimiter(func(ctx context.Context, ch string) (time.Duration, error) {
		admitted := false
		once.Do(func() { admitted = true })
		if admitted {
			return 0, nil
		}
		<-ctx.Done()
		return 0, ctx.Err()
	})
	for i := 0; i < 3; i++ {
		e.create(t, domain.ChannelSMS, "c")
	}
	s := testSettings()
	s.Concurrency, s.ClaimBatch = 3, 3
	r := start(t, worker.New(e.st, sender, lim, s, worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	<-entered
	waitFor(t, "two rows waiting on the limiter", func() bool { return e.count(t, "processing") == 3 })
	r.cancel()
	waitFor(t, "unstarted rows released", func() bool { return e.count(t, "pending") == 2 })
	select {
	case <-r.done:
		t.Fatal("Run returned while a send was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)
	r.wait(t)
	if n := e.count(t, "sent"); n != 1 {
		t.Fatalf("in-flight send must complete and be marked sent, sent=%d", n)
	}
	var bad int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE status = 'pending' AND (attempts <> 0 OR lease_token IS NOT NULL OR locked_until IS NOT NULL)`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("released rows must keep attempts 0 and have the lease cleared (bad=%d err=%v)", bad, err)
	}
}

func TestShutdownTimeoutCancelsStuckSend(t *testing.T) {
	e := newEnv(t)
	e.create(t, domain.ChannelSMS, "c")
	entered := make(chan struct{})
	sendCtxDone := make(chan struct{})
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		close(entered)
		<-ctx.Done()
		close(sendCtxDone)
		return provider.Result{Class: provider.Retryable, ErrCode: provider.CodeCanceled, Detail: "canceled"}
	})
	s := testSettings()
	s.ShutdownTimeout = 200 * time.Millisecond
	r := start(t, worker.New(e.st, sender, okLimiter, s, worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	<-entered
	began := time.Now()
	r.cancel()
	select {
	case <-sendCtxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck send was never cancelled")
	}
	if d := time.Since(began); d < 150*time.Millisecond {
		t.Fatalf("send was cancelled after %v, before the shutdown timeout", d)
	}
	r.wait(t)
}

func TestIdleBackoff(t *testing.T) {
	var (
		mu    sync.Mutex
		times []time.Time
	)
	q := &fakeQueue{onClaim: func() []store.QueueRow {
		mu.Lock()
		defer mu.Unlock()
		times = append(times, time.Now())
		if len(times) == 5 {
			return []store.QueueRow{{ID: "11111111-1111-1111-1111-111111111111", Channel: domain.ChannelSMS, MaxAttempts: 8, CreatedAt: time.Now()}}
		}
		return nil
	}}
	s := testSettings()
	s.IdleMin, s.IdleMax = 40*time.Millisecond, 160*time.Millisecond
	r := start(t, worker.New(q, funcSender(okSender), okLimiter, s, worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	waitFor(t, "enough claim calls", func() bool { mu.Lock(); defer mu.Unlock(); return len(times) >= 9 })
	r.cancel()
	r.wait(t)
	mu.Lock()
	defer mu.Unlock()
	gap := func(i int) time.Duration { return times[i+1].Sub(times[i]) }
	// Empty claims: gaps grow 40, 80, 160 and then stay at the 160ms cap.
	for i, atLeast := range []time.Duration{35, 75, 155, 155} {
		if g := gap(i); g < atLeast*time.Millisecond {
			t.Errorf("gap %d = %v, want at least %vms", i, g, atLeast)
		}
	}
	if g := gap(4); g > 30*time.Millisecond {
		t.Errorf("after a non-empty claim the next claim must be immediate, gap = %v", g)
	}
	if g := gap(5); g < 35*time.Millisecond || g >= 120*time.Millisecond {
		t.Errorf("backoff must reset to the minimum after a non-empty claim, gap = %v", g)
	}
}

func TestSuccessMarksSentAndPublishesNothingYet(t *testing.T) {
	e := newEnv(t)
	rdb := testRedis(t)
	ps := rdb.Subscribe(context.Background(), "notification-status")
	defer ps.Close()
	if _, err := ps.Receive(context.Background()); err != nil { // subscription confirmation
		t.Fatal(err)
	}
	lim := ratelimit.New(rdb, ratelimit.WithInterval(time.Millisecond), ratelimit.WithMaxWait(time.Second))
	obs := &recObserver{}
	id := e.create(t, domain.ChannelEmail, "c")
	start(t, worker.New(e.st, funcSender(okSender), lim, testSettings(), worker.WithRand(zeroRand), worker.WithObserver(obs)))
	waitFor(t, "sent", func() bool { return e.row(t, id).status == "sent" })
	r := e.row(t, id)
	if r.attempts != 1 || r.msgID != "msg-"+id[:8] || r.lastErr != "" || !r.leaseCleared {
		t.Fatalf("sent row wrong: %+v", r)
	}
	att, err := e.st.ListAttempts(context.Background(), id)
	if err != nil || len(att) != 1 || att[0].HTTPStatus == nil || *att[0].HTTPStatus != 202 || att[0].Error != nil {
		t.Fatalf("attempt rows wrong: %+v %v", att, err)
	}
	if msg, err := ps.ReceiveTimeout(context.Background(), 300*time.Millisecond); err == nil {
		t.Fatalf("nothing may be published on Redis yet, got %v", msg)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.sent != 1 || len(obs.attempts) != 1 || obs.attempts[0] != "email:success" {
		t.Fatalf("observer: sent=%d attempts=%v", obs.sent, obs.attempts)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, domain.ChannelSMS, "c")
	var mu sync.Mutex
	calls := 0
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return provider.Result{Class: provider.Retryable, HTTPStatus: 503, ErrCode: provider.Code5xx, Detail: "provider responded with status 503"}
		}
		return okSender(ctx, m)
	})
	start(t, worker.New(e.st, sender, okLimiter, testSettings(), worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	waitFor(t, "sent after retry", func() bool { return e.row(t, id).status == "sent" })
	if r := e.row(t, id); r.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", r.attempts)
	}
	att, err := e.st.ListAttempts(context.Background(), id)
	if err != nil || len(att) != 2 {
		t.Fatalf("want 2 attempt rows, got %d (%v)", len(att), err)
	}
	if att[0].Error == nil || !strings.HasPrefix(*att[0].Error, provider.Code5xx) {
		t.Fatalf("first attempt error wrong: %v", att[0].Error)
	}
}

func TestPermanentFailureStoresReasonAndNotifiesObserver(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, domain.ChannelPush, "c")
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		return provider.Result{Class: provider.Permanent, HTTPStatus: 400, ErrCode: provider.Code4xx, Detail: "provider responded with status 400"}
	})
	obs := &recObserver{}
	start(t, worker.New(e.st, sender, okLimiter, testSettings(), worker.WithRand(zeroRand), worker.WithObserver(obs)))
	waitFor(t, "failed", func() bool { return e.row(t, id).status == "failed" })
	if r := e.row(t, id); r.lastErr != "permanent: provider_4xx: provider responded with status 400" || r.attempts != 1 {
		t.Fatalf("failed row wrong: %+v", r)
	}
	waitFor(t, "observer notified", func() bool { obs.mu.Lock(); defer obs.mu.Unlock(); return len(obs.failed) == 1 })
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.failed[0] != "push:permanent" {
		t.Fatalf("observer failed = %v", obs.failed)
	}
}

func TestReaperLoopRecoversExpiredLease(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, domain.ChannelSMS, "c")
	rows, err := e.st.Claim(context.Background(), domain.ChannelSMS, 1, "00000000-0000-0000-0000-0000000000aa")
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v %d", err, len(rows))
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE notifications SET locked_until = now() - interval '1 second' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	start(t, worker.New(e.st, funcSender(okSender), okLimiter, testSettings(), worker.WithRand(zeroRand), worker.WithChannels(domain.ChannelSMS)))
	waitFor(t, "orphaned row recovered and sent", func() bool { return e.row(t, id).status == "sent" })
}

func TestReaperReportsFailedRowsToObserver(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, domain.ChannelSMS, "c")
	if _, err := e.st.Claim(context.Background(), domain.ChannelSMS, 1, "00000000-0000-0000-0000-0000000000bb"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE notifications SET locked_until = now() - interval '1 second', attempts = max_attempts WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	obs := &recObserver{}
	start(t, worker.New(e.st, funcSender(okSender), okLimiter, testSettings(), worker.WithRand(zeroRand), worker.WithObserver(obs)))
	waitFor(t, "failed by reaper", func() bool { return e.row(t, id).status == "failed" })
	waitFor(t, "observer notified", func() bool { obs.mu.Lock(); defer obs.mu.Unlock(); return len(obs.failed) == 1 })
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.failed[0] != "sms:exhausted" {
		t.Fatalf("observer failed = %v", obs.failed)
	}
}

func TestWorkerLogsCarryRowCorrelationID(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, domain.ChannelSMS, "corr-from-the-api-123")
	logs := &syncBuf{}
	start(t, worker.New(e.st, funcSender(okSender), okLimiter, testSettings(), worker.WithRand(zeroRand),
		worker.WithChannels(domain.ChannelSMS), worker.WithLogger(observability.NewLogger(logs, "debug"))))
	waitFor(t, "sent", func() bool { return e.row(t, id).status == "sent" })
	waitFor(t, "send log line", func() bool { return strings.Contains(logs.String(), `"notification_id":"`+id+`"`) })
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["notification_id"] != id {
			continue
		}
		found = true
		if rec["correlation_id"] != "corr-from-the-api-123" || rec["channel"] != "sms" {
			t.Errorf("row log line lacks correlation_id or channel: %v", rec)
		}
	}
	if !found {
		t.Fatal("no log line carried the notification id")
	}
	for _, secret := range []string{"+905551234567", "secret hello body"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leak %q", secret)
		}
	}
}

func TestUnknownLimiterErrorReleasesRow(t *testing.T) {
	e := newEnv(t)
	id := e.create(t, domain.ChannelSMS, "c")
	logs := &syncBuf{}
	var calls atomic.Int32
	secondAcquire := make(chan struct{})
	lim := funcLimiter(func(ctx context.Context, _ string) (time.Duration, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("redis down")
		}
		close(secondAcquire) // the released row was claimed again; hold it in Acquire until shutdown
		<-ctx.Done()
		return 0, ctx.Err()
	})
	r := start(t, worker.New(e.st, funcSender(okSender), lim, testSettings(), worker.WithRand(zeroRand),
		worker.WithChannels(domain.ChannelSMS), worker.WithLogger(observability.NewLogger(logs, "debug"))))
	waitFor(t, "ratelimit_error logged", func() bool { return strings.Contains(logs.String(), "ratelimit_error") })
	select {
	case <-secondAcquire:
	case <-time.After(15 * time.Second):
		t.Fatal("released row was never claimed again")
	}
	r.cancel()
	r.wait(t)
	if row := e.row(t, id); row.status != "pending" || row.attempts != 0 || !row.leaseCleared {
		t.Fatalf("row must be released untouched: %+v", row)
	}
}
