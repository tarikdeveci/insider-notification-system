package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

func TestPathValueSurvivesProblemMux(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.PathValue("id")))
	})
	rr := httptest.NewRecorder()
	problemMux{mux}.ServeHTTP(rr, httptest.NewRequest("GET", "/things/abc", nil))
	if rr.Code != 200 || rr.Body.String() != "abc" {
		t.Fatalf("code=%d body=%q", rr.Code, rr.Body)
	}
}

func TestRecoveredPanicLogsOneAccessRecordWithStatus500(t *testing.T) {
	const marker = "SENSITIVE-recipient-+905551234567"
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "info")
	h := wrap(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(marker) }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != 500 {
		t.Fatalf("code %d", rr.Code)
	}
	out := buf.String()
	if n := strings.Count(out, `"msg":"request"`); n != 1 {
		t.Fatalf("want 1 access record, got %d: %s", n, out)
	}
	if !strings.Contains(out, `"status":500`) || strings.Contains(out, marker) {
		t.Fatalf("log: %s", out)
	}
}
