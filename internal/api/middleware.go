package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

// RequestID echoes a valid client X-Request-ID or generates one, and stores it
// in the request context as the correlation id.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !observability.ValidRequestID(id) {
			id = observability.NewRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(observability.WithCorrelationID(r.Context(), id)))
	})
}

// Recover turns a handler panic into a 500 problem. Only the panic type is
// logged, never the value, which may carry sensitive data.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic recovered", observability.PanicAttrs(v)...)
				writeProblem(w, r, http.StatusInternalServerError, "internal error", nil)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog logs one record per request: method, matched route pattern (never the raw path or query), status and
// duration. Headers and bodies are never logged.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			log.InfoContext(r.Context(), "request",
				"method", r.Method,
				"route", routeLabel(r),
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// Instrument records request count and latency labelled with the mux pattern
// that served the request ("unmatched" for 404 and 405), never the raw path.
// It must receive the same *http.Request the mux serves, because the mux sets
// r.Pattern on it; nothing between Instrument and the mux may copy the request.
func Instrument(m *observability.APIMetrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			m.ObserveHTTP(metricMethod(r.Method), route, rec.status, time.Since(start))
		})
	}
}

// metricMethod bounds the method label: clients can send arbitrary tokens.
func metricMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return m
	}
	return "OTHER"
}

// routeLabel returns the mux pattern that served r, or "unmatched" when none did.
// Call it after the mux ran: the mux sets r.Pattern on the request it receives.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	return r.Pattern
}
