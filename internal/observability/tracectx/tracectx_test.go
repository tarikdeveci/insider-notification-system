package tracectx_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/tarikdeveci/insider-notification-system/internal/observability/tracectx"
)

func TestTraceParentRoundTrip(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "a")
	defer span.End()

	s := tracectx.TraceParent(ctx)
	if len(s) != 55 || s[:3] != "00-" {
		t.Fatalf("traceparent %q", s)
	}
	got := trace.SpanContextFromContext(tracectx.WithTraceParent(context.Background(), s))
	if !got.IsRemote() || got.TraceID() != span.SpanContext().TraceID() || got.SpanID() != span.SpanContext().SpanID() {
		t.Fatalf("round trip lost ids: %+v", got)
	}
}

func TestInvalidTraceParentIsIgnored(t *testing.T) {
	if tracectx.TraceParent(context.Background()) != "" {
		t.Fatal("no span means empty traceparent")
	}
	for _, bad := range []string{"", "garbage", "00-zz-zz-01", "00-00000000000000000000000000000000-0000000000000000-01"} {
		ctx := tracectx.WithTraceParent(context.Background(), bad)
		if trace.SpanContextFromContext(ctx).IsValid() {
			t.Errorf("%q must not produce a valid span context", bad)
		}
	}
}

func TestWithTraceParentDropsAmbientSpan(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	type key struct{}
	base := context.WithValue(context.Background(), key{}, "kept")
	ambient, span := tp.Tracer("t").Start(base, "ambient")
	defer span.End()
	for _, bad := range []string{"", "garbage"} {
		ctx := tracectx.WithTraceParent(ambient, bad)
		if trace.SpanContextFromContext(ctx).IsValid() {
			t.Errorf("%q must not leave the ambient span as parent", bad)
		}
		if ctx.Value(key{}) != "kept" {
			t.Errorf("%q: other context values must survive", bad)
		}
	}
}
