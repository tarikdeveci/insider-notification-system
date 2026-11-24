package worker_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
)

const (
	traceRecipient = "+905550001122"
	traceContent   = "tracing-secret-content"
)

type tracedRun struct {
	spans    []sdktrace.ReadOnlySpan
	id       string
	sendSpan trace.SpanContext // span context the sender saw
}

// postThenDeliver creates one notification through the real API handler, optionally
// rewrites its stored trace_parent, runs the worker until it is sent and returns every ended span.
func postThenDeliver(t *testing.T, rewriteTraceParent string) tracedRun {
	t.Helper()
	e := newEnv(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	h := api.NewServer(api.Deps{Store: e.st, DB: e.pool, Redis: nopPinger{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TracerProvider: tp})
	body := fmt.Sprintf(`{"recipient":%q,"channel":"sms","content":%q}`, traceRecipient, traceContent)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/notifications", strings.NewReader(body)))
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM notifications`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if rewriteTraceParent != "" {
		if _, err := e.pool.Exec(context.Background(), `UPDATE notifications SET trace_parent = $1`, rewriteTraceParent); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var seen trace.SpanContext
	sender := funcSender(func(ctx context.Context, m provider.Message) provider.Result {
		mu.Lock()
		seen = trace.SpanContextFromContext(ctx)
		mu.Unlock()
		return okSender(ctx, m)
	})
	run := start(t, worker.New(e.st, sender, okLimiter, testSettings(), worker.WithRand(zeroRand),
		worker.WithChannels(domain.ChannelSMS), worker.WithTracerProvider(tp)))
	waitFor(t, "sent", func() bool { return e.row(t, id).status == "sent" })
	run.cancel()
	run.wait(t)

	mu.Lock()
	defer mu.Unlock()
	return tracedRun{spans: rec.Ended(), id: id, sendSpan: seen}
}

type nopPinger struct{}

func (nopPinger) Ping(context.Context) error { return nil }

func find(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func TestWorkerSpanIsChildOfAPITrace(t *testing.T) {
	r := postThenDeliver(t, "")
	httpSpan, deliver := find(r.spans, "POST /v1/notifications"), find(r.spans, "deliver")
	if httpSpan == nil || deliver == nil {
		t.Fatalf("missing spans: %v", names(r.spans))
	}
	if deliver.SpanContext().TraceID() != httpSpan.SpanContext().TraceID() {
		t.Fatalf("deliver trace %s, api trace %s", deliver.SpanContext().TraceID(), httpSpan.SpanContext().TraceID())
	}
	if deliver.Parent().SpanID() != httpSpan.SpanContext().SpanID() || !deliver.Parent().IsRemote() {
		t.Fatalf("deliver parent %v, want remote %s", deliver.Parent(), httpSpan.SpanContext().SpanID())
	}
	send := find(r.spans, "provider.send")
	if send == nil || send.Parent().SpanID() != deliver.SpanContext().SpanID() {
		t.Fatalf("provider.send must be a child of deliver: %v", names(r.spans))
	}
	if r.sendSpan.SpanID() != send.SpanContext().SpanID() {
		t.Fatalf("the sender must run inside the provider.send span")
	}
}

func TestInvalidStoredTraceparentStartsNewRoot(t *testing.T) {
	r := postThenDeliver(t, "garbage")
	deliver := find(r.spans, "deliver")
	if deliver == nil {
		t.Fatalf("missing deliver span: %v", names(r.spans))
	}
	if deliver.Parent().IsValid() {
		t.Fatalf("invalid traceparent must give a root span, parent %v", deliver.Parent())
	}
	if deliver.SpanContext().TraceID() == find(r.spans, "POST /v1/notifications").SpanContext().TraceID() {
		t.Fatal("a root deliver span must start its own trace")
	}
}

func TestSpansCoverHTTPInsertClaimProviderCall(t *testing.T) {
	r := postThenDeliver(t, "")
	for _, want := range []string{"POST /v1/notifications", "db.insert_notification", "claim", "deliver", "provider.send"} {
		if find(r.spans, want) == nil {
			t.Errorf("missing span %q, have %v", want, names(r.spans))
		}
	}
	insert, httpSpan := find(r.spans, "db.insert_notification"), find(r.spans, "POST /v1/notifications")
	if insert != nil && httpSpan != nil && insert.Parent().SpanID() != httpSpan.SpanContext().SpanID() {
		t.Error("db.insert_notification must be a child of the http span")
	}
	if claim := find(r.spans, "claim"); claim != nil && claim.Parent().IsValid() {
		t.Error("claim is its own root")
	}
	if send := find(r.spans, "provider.send"); send != nil {
		got := map[string]string{}
		for _, a := range send.Attributes() {
			got[string(a.Key)] = a.Value.String()
		}
		if got["provider.result"] != "success" || got["http.response.status_code"] != "202" {
			t.Errorf("provider.send attributes: %v", got)
		}
	}
}

func TestSpansNeverCarrySensitiveValues(t *testing.T) {
	r := postThenDeliver(t, "")
	for _, s := range r.spans {
		var all []string
		all = append(all, s.Name(), s.Status().Description)
		for _, a := range s.Attributes() {
			all = append(all, string(a.Key), a.Value.String())
		}
		for _, ev := range s.Events() {
			all = append(all, ev.Name)
			for _, a := range ev.Attributes {
				all = append(all, string(a.Key), a.Value.String())
			}
		}
		joined := strings.Join(all, "\n")
		for _, secret := range []string{traceRecipient, traceContent, "http://", "https://"} {
			if strings.Contains(joined, secret) {
				t.Errorf("span %q leaks %q: %s", s.Name(), secret, joined)
			}
		}
	}
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}
