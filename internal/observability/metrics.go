package observability

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

// Each process owns a local registry; the global default registry is never used.

var allChannels = []domain.Channel{domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush}

var allPriorities = []domain.Priority{domain.PriorityHigh, domain.PriorityNormal, domain.PriorityLow}

var depthStates = []string{"ready", "scheduled", "processing"}

// Handler serves reg in the Prometheus text format.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

func registerRuntime(reg *prometheus.Registry) {
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// APIMetrics holds the series the API process emits.
type APIMetrics struct {
	reg      *prometheus.Registry
	created  *prometheus.CounterVec
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewAPIMetrics registers the API series and the Go and process collectors on reg.
func NewAPIMetrics(reg *prometheus.Registry) *APIMetrics {
	m := &APIMetrics{
		reg: reg,
		created: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifications_created_total", Help: "Notifications accepted by the API (idempotent replays excluded).",
		}, []string{"channel", "priority"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "HTTP requests by method, route pattern and status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request latency by method and route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	reg.MustRegister(m.created, m.requests, m.duration)
	registerRuntime(reg)
	return m
}

// Handler serves the API registry.
func (m *APIMetrics) Handler() http.Handler { return Handler(m.reg) }

// Created counts one newly created notification. A nil receiver is a no-op.
func (m *APIMetrics) Created(ch domain.Channel, p domain.Priority) {
	if m == nil {
		return
	}
	m.created.WithLabelValues(string(ch), p.String()).Inc()
}

// ObserveHTTP records one finished request. route must be a mux pattern or
// "unmatched", never a raw path.
func (m *APIMetrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(d.Seconds())
}

// WorkerMetrics holds the series the worker process emits and implements
// worker.Observer.
type WorkerMetrics struct {
	reg      *prometheus.Registry
	sent     *prometheus.CounterVec
	failed   *prometheus.CounterVec
	attempts *prometheus.CounterVec
	provider *prometheus.HistogramVec
	e2e      *prometheus.HistogramVec
	rlWait   *prometheus.HistogramVec
	unparsed prometheus.Counter
}

// NewWorkerMetrics registers the worker series and the Go and process
// collectors on reg. circuit_breaker_state is fixed at 0 (closed) per channel
// because the breaker is a tier 2 feature.
func NewWorkerMetrics(reg *prometheus.Registry) *WorkerMetrics {
	m := &WorkerMetrics{
		reg: reg,
		sent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifications_sent_total", Help: "Notifications delivered successfully.",
		}, []string{"channel"}),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifications_failed_total", Help: "Notifications that reached the failed state.",
		}, []string{"channel", "reason"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "delivery_attempts_total", Help: "Provider delivery attempts by classified result.",
		}, []string{"channel", "result"}),
		provider: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "provider_request_duration_seconds", Help: "Provider call latency.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"channel"}),
		e2e: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "notification_end_to_end_seconds", Help: "Time from creation to successful delivery.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600, 21600, 86400},
		}, []string{"channel"}),
		rlWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ratelimit_wait_seconds", Help: "Time spent waiting for rate limiter admission.",
			Buckets: []float64{.001, .01, .05, .1, .25, .5, 1, 2, 5, 10},
		}, []string{"channel"}),
		unparsed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "provider_unparsed_body_total", Help: "2xx provider responses whose body could not be parsed.",
		}),
	}
	breaker := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "circuit_breaker_state", Help: "Circuit breaker state per channel (0 closed, 1 open, 2 half open).",
	}, []string{"channel"})
	for _, ch := range allChannels {
		breaker.WithLabelValues(string(ch)).Set(0)
	}
	reg.MustRegister(m.sent, m.failed, m.attempts, m.provider, m.e2e, m.rlWait, m.unparsed, breaker)
	registerRuntime(reg)
	return m
}

// Handler serves the worker registry.
func (m *WorkerMetrics) Handler() http.Handler { return Handler(m.reg) }

// UnparsedBody is the counter handed to provider.WithUnparsedCounter.
func (m *WorkerMetrics) UnparsedBody() prometheus.Counter { return m.unparsed }

// Attempt records one provider call and its classified result.
func (m *WorkerMetrics) Attempt(channel string, result provider.Class, d time.Duration) {
	m.attempts.WithLabelValues(channel, result.String()).Inc()
	m.provider.WithLabelValues(channel).Observe(d.Seconds())
}

// Sent records a successful delivery and its creation to delivery latency.
func (m *WorkerMetrics) Sent(channel string, endToEnd time.Duration) {
	m.sent.WithLabelValues(channel).Inc()
	m.e2e.WithLabelValues(channel).Observe(endToEnd.Seconds())
}

// Failed records a terminal failure (permanent, exhausted or expired).
func (m *WorkerMetrics) Failed(channel, reason string) {
	m.failed.WithLabelValues(channel, reason).Inc()
}

// RateLimitWait records time spent waiting for limiter admission.
func (m *WorkerMetrics) RateLimitWait(channel string, d time.Duration) {
	m.rlWait.WithLabelValues(channel).Observe(d.Seconds())
}

// DepthSource reads queue depth; *store.Store satisfies it.
type DepthSource interface {
	QueueDepth(ctx context.Context) ([]store.DepthRow, error)
}

const (
	depthTTL          = 5 * time.Second
	depthQueryTimeout = 2 * time.Second
)

// DepthOption customizes the queue depth collector.
type DepthOption func(*depthCollector)

// WithDepthClock injects the clock used for the 5s cache (tests).
func WithDepthClock(f func() time.Time) DepthOption {
	return func(c *depthCollector) { c.now = f }
}

type depthKey struct {
	ch    domain.Channel
	prio  domain.Priority
	state string
}

type depthCollector struct {
	src  DepthSource
	log  *slog.Logger
	desc *prometheus.Desc
	now  func() time.Time

	mu        sync.Mutex
	lastTry   time.Time
	attempted bool
	values    map[depthKey]int // nil until the first successful refresh
}

// RegisterQueueDepth registers the queue_depth gauge on reg. Only the API
// process calls it. The query runs lazily on scrape, at most once per 5s; on
// error the previous values are kept and only the error type is logged.
func RegisterQueueDepth(reg *prometheus.Registry, src DepthSource, log *slog.Logger, opts ...DepthOption) {
	c := &depthCollector{
		src: src, log: log, now: time.Now,
		desc: prometheus.NewDesc("queue_depth",
			"Queued notifications by channel, priority and state (ready, scheduled, processing).",
			[]string{"channel", "priority", "state"}, nil),
	}
	for _, o := range opts {
		o(c)
	}
	reg.MustRegister(c)
}

func (c *depthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *depthCollector) Collect(out chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); !c.attempted || now.Sub(c.lastTry) >= depthTTL {
		c.attempted, c.lastTry = true, now
		c.refresh()
	}
	if c.values == nil {
		return
	}
	for _, ch := range allChannels {
		for _, p := range allPriorities {
			for _, st := range depthStates {
				out <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue,
					float64(c.values[depthKey{ch, p, st}]), string(ch), p.String(), st)
			}
		}
	}
}

func (c *depthCollector) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), depthQueryTimeout)
	defer cancel()
	rows, err := c.src.QueueDepth(ctx)
	if err != nil {
		c.log.Error("queue depth refresh failed", ErrAttrs(err)...)
		return
	}
	v := make(map[depthKey]int, len(rows))
	for _, r := range rows {
		v[depthKey{r.Channel, r.Priority, r.State}] = r.Count
	}
	c.values = v
}
