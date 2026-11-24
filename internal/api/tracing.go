package api

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tarikdeveci/insider-notification-system/internal/observability/tracectx"
)

// Trace starts one server span per request, continuing an incoming
// traceparent when the client sent one. The span is renamed to the mux
// pattern by problemMux once the route is known, so names stay low
// cardinality. Health, readiness and metrics probes are not traced. Only the
// method, route and status code are recorded, never the path, query, headers
// or body. A nil tp means the global provider.
func Trace(tp trace.TracerProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if tp == nil {
			tp = otel.GetTracerProvider()
		}
		tracer := tracectx.Tracer(tp)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz", "/readyz", "/metrics":
				next.ServeHTTP(w, r)
				return
			}
			method := metricMethod(r.Method)
			ctx, span := tracer.Start(tracectx.Extract(r.Context(), r.Header), "HTTP "+method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attribute.String("http.request.method", method)))
			defer span.End()

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))
			span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
			if rec.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}

// nameSpan renames the request span to the matched route and records it.
func nameSpan(r *http.Request, pattern string) {
	span := trace.SpanFromContext(r.Context())
	if pattern == "" {
		pattern = "unmatched"
	}
	span.SetName(pattern)
	span.SetAttributes(attribute.String("http.route", pattern))
}
