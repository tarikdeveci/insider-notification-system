package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newHealthServer(pg, rd error) http.Handler {
	return api.NewServer(api.Deps{DB: fakePinger{pg}, Redis: fakePinger{rd}, Logger: discardLogger()})
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func get(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	return do(h, "GET", path, hdr)
}

func TestHealthz(t *testing.T) {
	rr := get(newHealthServer(nil, nil), "/healthz", nil)
	if rr.Code != 200 {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestReadyz(t *testing.T) {
	rr := get(newHealthServer(nil, nil), "/readyz", nil)
	if rr.Code != 200 {
		t.Fatalf("ready: %d %s", rr.Code, rr.Body)
	}
	rr = get(newHealthServer(nil, errors.New("down")), "/readyz", nil)
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 503 || body.Status != "degraded" || body.Checks["redis"] != "fail" || body.Checks["postgres"] != "ok" {
		t.Fatalf("degraded: %d %+v", rr.Code, body)
	}
}

func TestRequestIDEchoedGeneratedAndSanitized(t *testing.T) {
	h := newHealthServer(nil, nil)
	if got := get(h, "/healthz", map[string]string{"X-Request-ID": "abc-123"}).Header().Get("X-Request-ID"); got != "abc-123" {
		t.Errorf("echo: %q", got)
	}
	if got := get(h, "/healthz", nil).Header().Get("X-Request-ID"); len(got) == 0 {
		t.Error("must generate when absent")
	}
	if got := get(h, "/healthz", map[string]string{"X-Request-ID": "bad id with spaces"}).Header().Get("X-Request-ID"); got == "bad id with spaces" || got == "" {
		t.Errorf("must replace invalid id, got %q", got)
	}
}

func TestRecoverReturnsProblem(t *testing.T) {
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := api.RequestID(api.Recover(discardLogger())(panicky))
	rr := get(h, "/x", nil)
	if rr.Code != 500 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("code=%d ct=%s", rr.Code, rr.Header().Get("Content-Type"))
	}
	if strings.Contains(rr.Body.String(), "boom") {
		t.Error("panic value must not leak to the client")
	}
}

func TestRecoverLogsTypeNotValue(t *testing.T) {
	const marker = "SENSITIVE-recipient-+905551234567"
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "info")
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(errors.New(marker)) })
	h := api.RequestID(api.Recover(log)(panicky))
	rr := get(h, "/x", map[string]string{"X-Request-ID": "rid-1"})
	if rr.Code != 500 {
		t.Fatalf("code %d", rr.Code)
	}
	if strings.Contains(buf.String(), marker) || strings.Contains(rr.Body.String(), marker) {
		t.Fatalf("marker leaked: log=%s body=%s", buf.String(), rr.Body)
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log not JSON: %v: %s", err, buf.String())
	}
	if rec["panic_type"] != "*errors.errorString" || rec["correlation_id"] != "rid-1" {
		t.Fatalf("record: %v", rec)
	}
	var p map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil || p["request_id"] != "rid-1" {
		t.Fatalf("problem body: %s", rr.Body)
	}
}

func TestAccessLogHasNoQueryString(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := api.AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	get(h, "/healthz?secret=abc", nil)
	if strings.Contains(buf.String(), "secret") || !strings.Contains(buf.String(), `"status":204`) {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestUnknownPathIsProblem404(t *testing.T) {
	rr := get(newHealthServer(nil, nil), "/nope", map[string]string{"X-Request-ID": "rid-404"})
	if rr.Code != 404 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("code=%d ct=%s", rr.Code, rr.Header().Get("Content-Type"))
	}
	if rr.Header().Get("X-Request-ID") != "rid-404" {
		t.Errorf("request id header: %q", rr.Header().Get("X-Request-ID"))
	}
	var p map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil || p["status"] != float64(404) || p["request_id"] != "rid-404" {
		t.Fatalf("body: %s", rr.Body)
	}
}

func TestWrongMethodIsProblem405WithAllow(t *testing.T) {
	rr := do(newHealthServer(nil, nil), "POST", "/healthz", map[string]string{"X-Request-ID": "rid-405"})
	if rr.Code != 405 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("code=%d ct=%s", rr.Code, rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(rr.Header().Get("Allow"), "GET") {
		t.Errorf("Allow header: %q", rr.Header().Get("Allow"))
	}
	if rr.Header().Get("X-Request-ID") != "rid-405" {
		t.Errorf("request id header: %q", rr.Header().Get("X-Request-ID"))
	}
	var p map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil || p["status"] != float64(405) {
		t.Fatalf("body: %s", rr.Body)
	}
}

func TestRoutePatterns(t *testing.T) {
	got := strings.Join(api.RoutePatterns(), ",")
	if !strings.Contains(got, "GET /healthz") || !strings.Contains(got, "GET /readyz") {
		t.Fatalf("patterns: %s", got)
	}
}
