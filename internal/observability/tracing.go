package observability

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

const tracingShutdownTimeout = 5 * time.Second

// NewTraceExporter builds the OTLP over HTTP exporter when
// OTEL_EXPORTER_OTLP_ENDPOINT (or the traces specific variant) is set, and
// returns nil, nil otherwise: nothing is exported and no connection is made.
// The exporter itself reads the endpoint and headers from the environment.
func NewTraceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return nil, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	return exp, nil
}

// SetupTracing installs the global tracer provider and the W3C TraceContext
// propagator for service ("notification-api" or "notification-worker").
// Without an endpoint the provider has no span processor: spans still get
// valid ids (so traceparent strings are stored and propagated) but nothing is
// exported. The returned shutdown flushes pending spans within a bounded time.
func SetupTracing(ctx context.Context, service string) (shutdown func(context.Context) error, err error) {
	exp, err := NewTraceExporter(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service)))
	if err != nil {
		return nil, fmt.Errorf("trace resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if exp != nil {
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tracingShutdownTimeout)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}
