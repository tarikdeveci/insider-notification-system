package observability_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

var _ worker.Observer = (*observability.WorkerMetrics)(nil)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 200 {
		t.Fatalf("scrape code %d", rr.Code)
	}
	return rr.Body.String()
}

// sample returns the value of the exposition line whose series (name plus
// labels) equals series.
func sample(t *testing.T, body, series string) (float64, bool) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, series+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, series+" ")), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v, true
		}
	}
	return 0, false
}

func mustSample(t *testing.T, body, series string) float64 {
	t.Helper()
	v, ok := sample(t, body, series)
	if !ok {
		t.Fatalf("series %s missing from output", series)
	}
	return v
}

func hasSeries(body, name string) bool {
	return strings.HasPrefix(body, name) || strings.Contains(body, "\n"+name)
}

type fakeDepth struct {
	mu   sync.Mutex
	rows []store.DepthRow
	err  error
	n    int
}

func (f *fakeDepth) QueueDepth(context.Context) ([]store.DepthRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.rows, f.err
}

func TestMetricsEndpointExposesRequiredSeries(t *testing.T) {
	apiReg, workerReg := prometheus.NewRegistry(), prometheus.NewRegistry()
	am := observability.NewAPIMetrics(apiReg)
	observability.RegisterQueueDepth(apiReg, &fakeDepth{rows: []store.DepthRow{
		{Channel: domain.ChannelSMS, Priority: domain.PriorityHigh, State: "ready", Count: 4},
	}}, discard())
	wm := observability.NewWorkerMetrics(workerReg)

	// One create, one HTTP request, one worker outcome of each kind.
	am.Created(domain.ChannelSMS, domain.PriorityHigh)
	am.ObserveHTTP("POST", "POST /v1/notifications", 201, 12*time.Millisecond)
	wm.Attempt("sms", provider.Success, 30*time.Millisecond)
	wm.Sent("sms", 2*time.Second)
	wm.Failed("email", "permanent")
	wm.RateLimitWait("push", 150*time.Millisecond)
	wm.UnparsedBody().Inc()

	apiOut := scrape(t, am.Handler())
	workerOut := scrape(t, wm.Handler())

	for _, name := range []string{
		"notifications_created_total", "http_requests_total", "http_request_duration_seconds_count",
		"queue_depth", "go_goroutines", "process_cpu_seconds_total",
	} {
		if !hasSeries(apiOut, name) {
			t.Errorf("api output lacks %s", name)
		}
	}
	for _, name := range []string{
		"notifications_sent_total", "notifications_failed_total", "delivery_attempts_total",
		"provider_request_duration_seconds_count", "notification_end_to_end_seconds_count",
		"ratelimit_wait_seconds_count", "provider_unparsed_body_total", "circuit_breaker_state",
		"go_goroutines", "process_cpu_seconds_total",
	} {
		if !hasSeries(workerOut, name) {
			t.Errorf("worker output lacks %s", name)
		}
	}
	// Each process registers only what it emits.
	if hasSeries(apiOut, "notifications_sent_total") || hasSeries(workerOut, "queue_depth") {
		t.Error("series leaked into the wrong process registry")
	}

	checks := []struct {
		out, series string
		want        float64
	}{
		{apiOut, `notifications_created_total{channel="sms",priority="high"}`, 1},
		{apiOut, `http_requests_total{method="POST",route="POST /v1/notifications",status="201"}`, 1},
		{apiOut, `queue_depth{channel="sms",priority="high",state="ready"}`, 4},
		{workerOut, `delivery_attempts_total{channel="sms",result="success"}`, 1},
		{workerOut, `notifications_sent_total{channel="sms"}`, 1},
		{workerOut, `notifications_failed_total{channel="email",reason="permanent"}`, 1},
		{workerOut, `provider_request_duration_seconds_count{channel="sms"}`, 1},
		{workerOut, `notification_end_to_end_seconds_sum{channel="sms"}`, 2},
		{workerOut, `ratelimit_wait_seconds_count{channel="push"}`, 1},
		{workerOut, `provider_unparsed_body_total`, 1},
		{workerOut, `circuit_breaker_state{channel="sms"}`, 0},
		{workerOut, `circuit_breaker_state{channel="email"}`, 0},
		{workerOut, `circuit_breaker_state{channel="push"}`, 0},
	}
	for _, c := range checks {
		if got := mustSample(t, c.out, c.series); got != c.want {
			t.Errorf("%s = %v want %v", c.series, got, c.want)
		}
	}
}

func TestQueueDepthEmitsEveryCombinationWithZeros(t *testing.T) {
	reg := prometheus.NewRegistry()
	src := &fakeDepth{rows: []store.DepthRow{
		{Channel: domain.ChannelEmail, Priority: domain.PriorityLow, State: "scheduled", Count: 7},
	}}
	observability.RegisterQueueDepth(reg, src, discard())
	out := scrape(t, observability.Handler(reg))
	if n := strings.Count(out, "\nqueue_depth{"); n != 27 {
		t.Fatalf("want 27 queue_depth series, got %d", n)
	}
	if v := mustSample(t, out, `queue_depth{channel="email",priority="low",state="scheduled"}`); v != 7 {
		t.Fatalf("got %v", v)
	}
	if v := mustSample(t, out, `queue_depth{channel="sms",priority="normal",state="processing"}`); v != 0 {
		t.Fatalf("got %v", v)
	}
}

func TestQueueDepthKeepsLastValuesOnError(t *testing.T) {
	reg := prometheus.NewRegistry()
	src := &fakeDepth{rows: []store.DepthRow{{Channel: domain.ChannelSMS, Priority: domain.PriorityNormal, State: "ready", Count: 3}}}
	now := time.Unix(1000, 0)
	observability.RegisterQueueDepth(reg, src, discard(), observability.WithDepthClock(func() time.Time { return now }))
	h := observability.Handler(reg)
	scrape(t, h)
	src.mu.Lock()
	src.err = errors.New("db down")
	src.mu.Unlock()
	now = now.Add(6 * time.Second)
	out := scrape(t, h)
	if v := mustSample(t, out, `queue_depth{channel="sms",priority="normal",state="ready"}`); v != 3 {
		t.Fatalf("last value lost: %v", v)
	}
	if src.n != 2 {
		t.Fatalf("source calls %d", src.n)
	}
}

// Before the first successful refresh there are no real values, so queue_depth
// is absent rather than a run of zeros that would read as an empty queue during
// a database outage. After recovery all 27 combinations appear.
func TestQueueDepthAbsentUntilFirstSuccessfulRefresh(t *testing.T) {
	reg := prometheus.NewRegistry()
	src := &fakeDepth{err: errors.New("db down"),
		rows: []store.DepthRow{{Channel: domain.ChannelPush, Priority: domain.PriorityHigh, State: "processing", Count: 2}}}
	now := time.Unix(1000, 0)
	observability.RegisterQueueDepth(reg, src, discard(), observability.WithDepthClock(func() time.Time { return now }))
	h := observability.Handler(reg)
	if out := scrape(t, h); hasSeries(out, "queue_depth") {
		t.Fatal("queue_depth emitted before any successful refresh")
	}
	src.mu.Lock()
	src.err = nil
	src.mu.Unlock()
	now = now.Add(6 * time.Second)
	out := scrape(t, h)
	if n := strings.Count(out, "\nqueue_depth{"); n != 27 {
		t.Fatalf("want 27 queue_depth series after recovery, got %d", n)
	}
	if v := mustSample(t, out, `queue_depth{channel="push",priority="high",state="processing"}`); v != 2 {
		t.Fatalf("got %v", v)
	}
}

type countingDepth struct {
	inner *store.Store
	n     int
}

func (c *countingDepth) QueueDepth(ctx context.Context) ([]store.DepthRow, error) {
	c.n++
	return c.inner.QueueDepth(ctx)
}

func TestQueueDepthGaugeStatesReadyScheduledProcessing(t *testing.T) {
	pool := testutil.MigratedPool(t)
	st := store.New(pool)
	ctx := context.Background()

	mk := func(ch domain.Channel, to string, prio domain.Priority) domain.Notification {
		t.Helper()
		res, err := st.CreateNotification(ctx, store.CreateParams{
			New:           domain.NewNotification{Channel: ch, Recipient: to, Content: "c", Priority: prio},
			CorrelationID: "t",
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Notification
	}
	mk(domain.ChannelSMS, "+905551234567", domain.PriorityHigh)
	fut := mk(domain.ChannelSMS, "+905551234567", domain.PriorityHigh)
	if _, err := pool.Exec(ctx, `UPDATE notifications SET next_attempt_at = now() + interval '1 hour' WHERE id = $1::uuid`, fut.ID); err != nil {
		t.Fatal(err)
	}
	mk(domain.ChannelEmail, "a@example.com", domain.PriorityLow)
	if rows, err := st.Claim(ctx, domain.ChannelEmail, 1, "11111111-1111-4111-8111-111111111111"); err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v %d", err, len(rows))
	}

	counting := &countingDepth{inner: st}
	now := time.Unix(5000, 0)
	reg := prometheus.NewRegistry()
	observability.RegisterQueueDepth(reg, counting, discard(), observability.WithDepthClock(func() time.Time { return now }))
	h := observability.Handler(reg)

	out := scrape(t, h)
	want := map[string]float64{
		`queue_depth{channel="sms",priority="high",state="ready"}`:         1,
		`queue_depth{channel="sms",priority="high",state="scheduled"}`:     1,
		`queue_depth{channel="sms",priority="high",state="processing"}`:    0,
		`queue_depth{channel="email",priority="low",state="processing"}`:   1,
		`queue_depth{channel="push",priority="normal",state="ready"}`:      0,
		`queue_depth{channel="email",priority="normal",state="scheduled"}`: 0,
	}
	for series, v := range want {
		if got := mustSample(t, out, series); got != v {
			t.Errorf("%s = %v want %v", series, got, v)
		}
	}
	if counting.n != 1 {
		t.Fatalf("one grouped query expected, got %d", counting.n)
	}

	// Within 5s the cache answers; after 5s the query runs again.
	mk(domain.ChannelPush, "tok123", domain.PriorityNormal)
	now = now.Add(4 * time.Second)
	out = scrape(t, h)
	if counting.n != 1 || mustSample(t, out, `queue_depth{channel="push",priority="normal",state="ready"}`) != 0 {
		t.Fatalf("cache not honoured: calls=%d", counting.n)
	}
	now = now.Add(2 * time.Second)
	out = scrape(t, h)
	if counting.n != 2 || mustSample(t, out, `queue_depth{channel="push",priority="normal",state="ready"}`) != 1 {
		t.Fatalf("refresh missing: calls=%d", counting.n)
	}
}

func TestBreakerStateMetric(t *testing.T) {
	wm := observability.NewWorkerMetrics(prometheus.NewRegistry())
	series := func(ch string) string { return `circuit_breaker_state{channel="` + ch + `"}` }
	for _, step := range []int{0, 1, 2, 0} {
		wm.BreakerState("sms", step)
		out := scrape(t, wm.Handler())
		if got := mustSample(t, out, series("sms")); got != float64(step) {
			t.Fatalf("after BreakerState(sms, %d) gauge = %v", step, got)
		}
		for _, other := range []string{"email", "push"} {
			if got := mustSample(t, out, series(other)); got != 0 {
				t.Fatalf("%s gauge = %v, want 0 (channels are independent)", other, got)
			}
		}
	}
}
