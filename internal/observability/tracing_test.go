package observability_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

func restoreGlobals(t *testing.T) {
	t.Helper()
	tp, prop := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(prop)
	})
}

func TestExporterIsNoopWithoutEndpoint(t *testing.T) {
	restoreGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	if exp, err := observability.NewTraceExporter(context.Background()); err != nil || exp != nil {
		t.Fatalf("no endpoint must build no exporter, got %v %v", exp, err)
	}
	shutdown, err := observability.SetupTracing(context.Background(), "notification-test")
	if err != nil {
		t.Fatal(err)
	}
	// Spans still get valid ids, so traceparent strings are produced without exporting anything.
	ctx, span := otel.Tracer("t").Start(context.Background(), "x")
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	span.End()
	if carrier.Get("traceparent") == "" {
		t.Fatal("traceparent must still be produced without an endpoint")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestExporterBuiltWhenEndpointSet(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	exp, err := observability.NewTraceExporter(context.Background())
	if err != nil || exp == nil {
		t.Fatalf("endpoint set must build an exporter, got %v %v", exp, err)
	}
	_ = exp.Shutdown(context.Background())
}
