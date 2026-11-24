package api_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func newMetricsAPI(t *testing.T) http.Handler {
	t.Helper()
	pool := testutil.MigratedPool(t)
	m := observability.NewAPIMetrics(prometheus.NewRegistry())
	return api.NewServer(api.Deps{Store: store.New(pool), DB: pool, Logger: discardLogger(), Metrics: m})
}

func metricsBody(t *testing.T, h http.Handler) string {
	t.Helper()
	rr := call(t, h, "GET", "/metrics", "", nil)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics: %d %s", rr.Code, rr.Header().Get("Content-Type"))
	}
	return rr.Body.String()
}

func TestHTTPMetricsLabelsUseRoutePattern(t *testing.T) {
	h := newMetricsAPI(t)
	created := call(t, h, "POST", "/v1/notifications", smsBody, nil)
	if created.Code != 201 {
		t.Fatalf("create: %d", created.Code)
	}
	id := obj(t, created)["id"].(string)
	call(t, h, "GET", "/v1/notifications/"+id, "", nil)
	call(t, h, "GET", "/v1/notifications/not-a-uuid", "", nil)
	call(t, h, "GET", "/no/such/path/secret-token-123", "", nil)
	call(t, h, "DELETE", "/v1/notifications", "", nil)

	out := metricsBody(t, h)
	for _, want := range []string{
		`http_requests_total{method="POST",route="POST /v1/notifications",status="201"} 1`,
		`http_requests_total{method="GET",route="GET /v1/notifications/{id}",status="200"} 1`,
		`http_requests_total{method="GET",route="GET /v1/notifications/{id}",status="404"} 1`,
		`http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`http_requests_total{method="DELETE",route="unmatched",status="405"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, id) || strings.Contains(out, "secret-token-123") || strings.Contains(out, "not-a-uuid") {
		t.Error("raw path leaked into a label")
	}
	routes := regexp.MustCompile(`route="([^"]*)"`).FindAllStringSubmatch(out, -1)
	known := map[string]bool{"unmatched": true}
	for _, p := range api.RoutePatterns() {
		known[p] = true
	}
	for _, m := range routes {
		if !known[m[1]] {
			t.Errorf("route label %q is not a registered pattern", m[1])
		}
	}
}

func TestCreatedCounterCountsNewItemsOnly(t *testing.T) {
	h := newMetricsAPI(t)
	key := map[string]string{"Idempotency-Key": "k1"}
	call(t, h, "POST", "/v1/notifications", smsBody, key)
	call(t, h, "POST", "/v1/notifications", smsBody, key) // replay
	bkey := map[string]string{"Idempotency-Key": "b1"}
	call(t, h, "POST", "/v1/notifications/batch", batchJSON(3), bkey)
	call(t, h, "POST", "/v1/notifications/batch", batchJSON(3), bkey)                              // replay
	call(t, h, "POST", "/v1/notifications", `{"recipient":"x","channel":"sms","content":""}`, nil) // 422

	out := metricsBody(t, h)
	want := `notifications_created_total{channel="sms",priority="normal"} 4`
	if !strings.Contains(out, want) {
		t.Fatalf("want %q in\n%s", want, out)
	}
}
