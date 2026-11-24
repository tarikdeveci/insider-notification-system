package api_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func newTracedAPI(t *testing.T) (http.Handler, *pgxpool.Pool, *tracetest.SpanRecorder) {
	t.Helper()
	pool := testutil.MigratedPool(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	h := api.NewServer(api.Deps{
		Store: store.New(pool), DB: pool, Redis: fakePinger{}, Logger: discardLogger(), TracerProvider: tp,
	})
	return h, pool, rec
}

var traceparentRE = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)

func TestAPIStoresTraceparentOnRow(t *testing.T) {
	h, pool, rec := newTracedAPI(t)
	rr := call(t, h, "POST", "/v1/notifications", smsBody, nil)
	if rr.Code != 201 {
		t.Fatalf("code %d: %s", rr.Code, rr.Body)
	}
	id := obj(t, rr)["id"].(string)

	var tp *string
	if err := pool.QueryRow(context.Background(), `SELECT trace_parent FROM notifications WHERE id = $1::uuid`, id).Scan(&tp); err != nil {
		t.Fatal(err)
	}
	if tp == nil || !traceparentRE.MatchString(*tp) {
		t.Fatalf("trace_parent not stored or malformed: %v", tp)
	}
	var httpSpan sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "POST /v1/notifications" {
			httpSpan = s
		}
	}
	if httpSpan == nil {
		t.Fatalf("no http span among %v", spanNames(rec))
	}
	want := "00-" + httpSpan.SpanContext().TraceID().String() + "-" + httpSpan.SpanContext().SpanID().String() + "-01"
	if *tp != want {
		t.Fatalf("trace_parent %q, want the http span %q", *tp, want)
	}
	if strings.Contains(rr.Body.String(), "trace_parent") || strings.Contains(rr.Body.String(), *tp) {
		t.Fatalf("trace_parent must not be exposed: %s", rr.Body)
	}
}

func TestAPIBatchRowsShareTraceparent(t *testing.T) {
	h, pool, _ := newTracedAPI(t)
	body := batchJSON(2)
	rr := call(t, h, "POST", "/v1/notifications/batch", body, nil)
	if rr.Code != 201 && rr.Code != 202 {
		t.Fatalf("code %d: %s", rr.Code, rr.Body)
	}
	var n, distinct int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), count(DISTINCT trace_parent) FROM notifications WHERE trace_parent IS NOT NULL`).Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 2 || distinct != 1 {
		t.Fatalf("want 2 rows sharing one trace_parent, got n=%d distinct=%d", n, distinct)
	}
}

func TestHTTPSpanNameIsRoutePatternAndHealthIsUntraced(t *testing.T) {
	h, _, rec := newTracedAPI(t)
	call(t, h, "GET", "/v1/notifications/00000000-0000-0000-0000-000000000001", "", nil)
	call(t, h, "GET", "/nope/abc123", "", nil)
	call(t, h, "GET", "/healthz", "", nil)
	names := strings.Join(spanNames(rec), "|")
	if !strings.Contains(names, "GET /v1/notifications/{id}") || strings.Contains(names, "0000-0000") {
		t.Errorf("span names must use the route pattern: %s", names)
	}
	if strings.Contains(names, "abc123") || strings.Contains(names, "healthz") {
		t.Errorf("raw paths and health probes must not become spans: %s", names)
	}
}

func spanNames(rec *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range rec.Ended() {
		out = append(out, s.Name())
	}
	return out
}
