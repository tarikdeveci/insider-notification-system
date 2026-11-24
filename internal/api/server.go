// Package api is the HTTP layer: routing, middleware, problem+json and handlers.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

// Pinger is a dependency that can report liveness.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RedisPinger adapts a go-redis client to Pinger.
type RedisPinger struct{ Client *redis.Client }

// Ping returns the result of PING.
func (p RedisPinger) Ping(ctx context.Context) error { return p.Client.Ping(ctx).Err() }

// Deps are the collaborators the handlers need.
type Deps struct {
	Store  *store.Store
	DB     Pinger
	Redis  Pinger
	Logger *slog.Logger
	// Metrics is optional; nil disables instrumentation and makes /metrics a 404.
	Metrics *observability.APIMetrics
	// Now is the clock used for schedule checks; nil means time.Now.
	Now func() time.Time
}

type route struct {
	pattern string
	handler http.HandlerFunc
}

type server struct{ Deps }

func (s *server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *server) routes() []route {
	return []route{
		{"GET /healthz", s.healthz},
		{"GET /readyz", s.readyz},
		{"GET /metrics", s.metrics},
		{"POST /v1/notifications", s.createNotification},
		{"GET /v1/notifications/{id}", s.getNotification},
		{"GET /v1/notifications", s.listNotifications},
		{"POST /v1/notifications/batch", s.createBatch},
		{"POST /v1/notifications/{id}/cancel", s.cancelNotification},
		{"POST /v1/notifications/{id}/retry", s.retryNotification},
		{"POST /v1/templates", s.createTemplate},
		{"GET /v1/templates/{id}", s.getTemplate},
		{"GET /v1/templates", s.listTemplates},
		{"GET /v1/batches/{id}", s.getBatch},
		{"POST /v1/batches/{id}/cancel", s.cancelBatch},
	}
}

// RoutePatterns lists the registered "METHOD /path" patterns.
func RoutePatterns() []string {
	var out []string
	for _, rt := range (&server{}).routes() {
		out = append(out, rt.pattern)
	}
	return out
}

// NewServer builds the handler: RequestID -> AccessLog -> Recover -> mux.
func NewServer(d Deps) http.Handler {
	s := &server{d}
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, rt.handler)
	}
	return wrapWith(d.Logger, d.Metrics, problemMux{mux})
}

// wrap applies the middleware chain. Recover sits inside AccessLog so a
// recovered panic still produces exactly one access record, with status 500.
func wrap(log *slog.Logger, h http.Handler) http.Handler { return wrapWith(log, nil, h) }

// wrapWith is wrap plus request metrics, which sit between AccessLog and
// Recover so a recovered panic is still counted, with status 500.
func wrapWith(log *slog.Logger, m *observability.APIMetrics, h http.Handler) http.Handler {
	return RequestID(AccessLog(log)(Instrument(m)(Recover(log)(h))))
}

// problemMux renders the mux's own 404 and 405 answers as problem+json,
// keeping the Allow header on 405. A catch-all route is not used because it
// would swallow the mux's method matching and hide every 405.
type problemMux struct{ mux *http.ServeMux }

func (p problemMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, pattern := p.mux.Handler(r)
	if pattern != "" {
		// Dispatch through the mux itself so it populates PathValue.
		p.mux.ServeHTTP(w, r)
		return
	}
	probe := &probeWriter{header: http.Header{}}
	h.ServeHTTP(probe, r)
	switch probe.status {
	case http.StatusNotFound:
		writeProblem(w, r, http.StatusNotFound, "no such route", nil)
	case http.StatusMethodNotAllowed:
		if allow := probe.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeProblem(w, r, http.StatusMethodNotAllowed, "method not allowed", nil)
	default: // e.g. trailing slash redirects
		h.ServeHTTP(w, r)
	}
}

// probeWriter captures status and headers of the mux's default handlers.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header         { return p.header }
func (p *probeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.Metrics == nil {
		writeProblem(w, r, http.StatusNotFound, "metrics are not enabled", nil)
		return
	}
	s.Metrics.Handler().ServeHTTP(w, r)
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	check := func(p Pinger) string {
		if p == nil || p.Ping(ctx) != nil {
			return "fail"
		}
		return "ok"
	}
	pg, rd := check(s.DB), check(s.Redis)
	status, code := "ok", http.StatusOK
	if pg != "ok" || rd != "ok" {
		status, code = "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status": status,
		"checks": map[string]string{"postgres": pg, "redis": rd},
	})
}
