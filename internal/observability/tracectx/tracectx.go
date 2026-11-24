// Package tracectx holds the small tracing helpers shared by the store, the
// API and the worker. It imports no other project package, so the store can
// use it without an import cycle.
package tracectx

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const scope = "github.com/tarikdeveci/insider-notification-system"

var propagator = propagation.TraceContext{}

// TraceParent returns the W3C traceparent of the active span in ctx, or ""
// when there is none (or it is not valid).
func TraceParent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	propagator.Inject(ctx, c)
	return c.Get("traceparent")
}

// WithTraceParent returns ctx whose only span parent is tp as a remote span.
// Any span already in ctx is dropped first (other values and cancellation are
// kept), so an empty or malformed tp makes the next span a new root instead of
// inheriting an unrelated caller trace.
func WithTraceParent(ctx context.Context, tp string) context.Context {
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	if tp == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": tp})
}

// Inject writes the traceparent of ctx into h (an outgoing request).
func Inject(ctx context.Context, h map[string][]string) {
	propagator.Inject(ctx, propagation.HeaderCarrier(h))
}

// Extract returns ctx carrying the remote span context found in h.
func Extract(ctx context.Context, h map[string][]string) context.Context {
	return propagator.Extract(ctx, propagation.HeaderCarrier(h))
}

// Start starts a child span with the provider of the span already in ctx. With
// no span in ctx the provider is a no-op, so untraced callers pay nothing.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(scope).Start(ctx, name, opts...)
}

// Tracer returns the tracer of tp under the project scope.
func Tracer(tp trace.TracerProvider) trace.Tracer { return tp.Tracer(scope) }
