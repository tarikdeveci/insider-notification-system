package worker_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability/tracectx"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

func recorderProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, rec
}

// runWith runs eng on runCtx until cond holds, then stops it and waits.
func runWith(t *testing.T, eng *worker.Engine, runCtx context.Context, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(runCtx)
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, what, cond)
}

func TestAmbientSpanNeverParentsWorkerSpans(t *testing.T) {
	e := newEnv(t)
	tp, rec := recorderProvider(t)
	ambientCtx, ambient := tp.Tracer("ambient").Start(context.Background(), "ambient")
	defer ambient.End()

	id := e.create(t, domain.ChannelSMS, "legacy") // no trace_parent: a legacy row
	eng := worker.New(e.st, funcSender(okSender), okLimiter, testSettings(), worker.WithRand(zeroRand),
		worker.WithChannels(domain.ChannelSMS), worker.WithTracerProvider(tp))
	runWith(t, eng, ambientCtx, "sent", func() bool { return e.row(t, id).status == "sent" })

	for _, name := range []string{"claim", "deliver"} {
		s := find(rec.Ended(), name)
		if s == nil {
			t.Fatalf("missing %s span: %v", name, names(rec.Ended()))
		}
		if s.Parent().IsValid() || s.SpanContext().TraceID() == ambient.SpanContext().TraceID() {
			t.Errorf("%s inherited the ambient span: parent %v", name, s.Parent())
		}
	}
}

func TestEmptyPollRecordsNoClaimSpan(t *testing.T) {
	e := newEnv(t)
	tp, rec := recorderProvider(t)
	var mu sync.Mutex
	polls := 0
	q := countingQueue{Queue: e.st, onClaim: func() { mu.Lock(); polls++; mu.Unlock() }}
	eng := worker.New(q, funcSender(okSender), okLimiter, testSettings(),
		worker.WithChannels(domain.ChannelSMS), worker.WithTracerProvider(tp))
	runWith(t, eng, context.Background(), "several empty polls", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return polls >= 5
	})
	if s := find(rec.Ended(), "claim"); s != nil {
		t.Fatalf("empty polls must not record claim spans, got %v", names(rec.Ended()))
	}
}

type countingQueue struct {
	worker.Queue
	onClaim func()
}

func (q countingQueue) ClaimWithAge(ctx context.Context, ch domain.Channel, n int, token string, maxAge time.Duration) ([]store.QueueRow, error) {
	q.onClaim()
	return q.Queue.ClaimWithAge(ctx, ch, n, token, maxAge)
}

func TestProviderRequestCarriesProviderSendTraceparent(t *testing.T) {
	e := newEnv(t)
	tp, rec := recorderProvider(t)
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("traceparent")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"messageId":"m-1"}`))
	}))
	defer srv.Close()
	client, err := provider.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	id := e.create(t, domain.ChannelSMS, "c")
	eng := worker.New(e.st, client, okLimiter, testSettings(), worker.WithRand(zeroRand),
		worker.WithChannels(domain.ChannelSMS), worker.WithTracerProvider(tp))
	runWith(t, eng, context.Background(), "sent", func() bool { return e.row(t, id).status == "sent" })

	send := find(rec.Ended(), "provider.send")
	if send == nil {
		t.Fatalf("missing provider.send: %v", names(rec.Ended()))
	}
	mu.Lock()
	defer mu.Unlock()
	sc := trace.SpanContextFromContext(tracectx.WithTraceParent(context.Background(), got))
	if !sc.IsValid() || sc.SpanID() != send.SpanContext().SpanID() || sc.TraceID() != send.SpanContext().TraceID() {
		t.Fatalf("outgoing traceparent %q does not identify provider.send %s", got, send.SpanContext().SpanID())
	}
}
