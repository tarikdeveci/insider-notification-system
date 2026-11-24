package contract_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

// sample is one request sent to the real handler. name is "METHOD path status"
// with the path as written in the spec. badRequest marks a request that is
// invalid on purpose (so the spec itself rejects it); its request validation is
// skipped, but its response is still validated.
type sample struct {
	name       string
	method     string
	path       string
	body       string
	header     map[string]string
	want       int
	badRequest bool
}

func TestSampleResponsesValidateAgainstSchemas(t *testing.T) {
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	pool := testutil.MigratedPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	newHandler := func(redis error) http.Handler {
		return api.NewServer(api.Deps{
			Store:   store.New(pool),
			DB:      pool,
			Redis:   pinger{err: redis},
			Logger:  logger,
			Metrics: observability.NewAPIMetrics(prometheus.NewRegistry()),
		})
	}
	handler := newHandler(nil)

	// do sends one sample, validates request and response against the spec and
	// returns the decoded JSON body (nil when the body is not a JSON object).
	do := func(t *testing.T, h http.Handler, s sample) map[string]any {
		t.Helper()
		var rd io.Reader
		if s.body != "" {
			rd = strings.NewReader(s.body)
		}
		req := httptest.NewRequest(s.method, "http://127.0.0.1:8080"+s.path, rd)
		if s.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range s.header {
			req.Header.Set(k, v)
		}
		route, pathParams, err := router.FindRoute(req)
		if err != nil {
			t.Fatalf("find route: %v", err)
		}
		in := &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: pathParams,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		if !s.badRequest {
			if err := openapi3filter.ValidateRequest(t.Context(), in); err != nil {
				t.Fatalf("request does not match spec: %v", err)
			}
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != s.want {
			t.Fatalf("status %d, want %d, body %s", rr.Code, s.want, rr.Body)
		}
		out := rr.Body.Bytes()
		err = openapi3filter.ValidateResponse(t.Context(), &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in,
			Status:                 rr.Code,
			Header:                 rr.Header(),
			Body:                   io.NopCloser(strings.NewReader(string(out))),
			Options:                &openapi3filter.Options{IncludeResponseStatus: true},
		})
		if err != nil {
			t.Fatalf("response does not match spec: %v\nbody: %s", err, out)
		}
		if rr.Code >= 400 && s.path != "/readyz" && !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
			t.Fatalf("error content type %q", rr.Header().Get("Content-Type"))
		}
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		return m
	}
	run := func(t *testing.T, s sample) map[string]any {
		var m map[string]any
		t.Run(s.name, func(t *testing.T) { m = do(t, handler, s) })
		return m
	}
	str := func(t *testing.T, m map[string]any, k string) string {
		t.Helper()
		v, ok := m[k].(string)
		if !ok || v == "" {
			t.Fatalf("field %q missing in %v", k, m)
		}
		return v
	}

	const sms = `{"recipient":"+905551234567","channel":"sms","content":"hello"}`
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	// Health, readiness, metrics.
	run(t, sample{name: "GET /healthz 200", method: "GET", path: "/healthz", want: 200})
	run(t, sample{name: "GET /readyz 200", method: "GET", path: "/readyz", want: 200})
	t.Run("GET /readyz 503", func(t *testing.T) {
		do(t, newHandler(errors.New("down")), sample{method: "GET", path: "/readyz", want: 503})
	})
	run(t, sample{name: "GET /metrics 200", method: "GET", path: "/metrics", want: 200})

	// Notifications: create, replay, conflict.
	idem := map[string]string{"Idempotency-Key": "contract-key-1"}
	n := run(t, sample{name: "POST /v1/notifications 201", method: "POST", path: "/v1/notifications", body: sms, header: idem, want: 201})
	id := str(t, n, "id")
	run(t, sample{name: "POST /v1/notifications 200 replay", method: "POST", path: "/v1/notifications", body: sms, header: idem, want: 200})
	run(t, sample{name: "POST /v1/notifications 409 idempotency conflict", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"+905551234567","channel":"sms","content":"other"}`, header: idem, want: 409})
	run(t, sample{name: "POST /v1/notifications 201 scheduled", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"+905551234567","channel":"sms","content":"later","priority":"high","scheduled_at":"` + future + `"}`, want: 201})
	run(t, sample{name: "POST /v1/notifications 201 email", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"a@example.com","channel":"email","content":"hi","priority":"low"}`, want: 201})
	run(t, sample{name: "POST /v1/notifications 400 malformed json", method: "POST", path: "/v1/notifications",
		body: `{"recipient":`, want: 400, badRequest: true})
	run(t, sample{name: "POST /v1/notifications 400 unknown field", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"+905551234567","channel":"sms","content":"x","bogus":1}`, want: 400, badRequest: true})
	run(t, sample{name: "POST /v1/notifications 400 bad idempotency key", method: "POST", path: "/v1/notifications",
		body: sms, header: map[string]string{"Idempotency-Key": "has space"}, want: 400, badRequest: true})
	run(t, sample{name: "POST /v1/notifications 413 oversize", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"+905551234567","channel":"sms","content":"` + strings.Repeat("a", 70*1024) + `"}`, want: 413, badRequest: true})
	run(t, sample{name: "POST /v1/notifications 422 validation", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"nope","channel":"sms","content":""}`, want: 422})

	// Templates.
	tpl := run(t, sample{name: "POST /v1/templates 201", method: "POST", path: "/v1/templates",
		body: `{"name":"welcome","channel":"sms","body":"Hi {{name}}"}`, want: 201})
	tplID := str(t, tpl, "id")
	run(t, sample{name: "POST /v1/templates 409 duplicate name", method: "POST", path: "/v1/templates",
		body: `{"name":"welcome","channel":"sms","body":"Other"}`, want: 409})
	run(t, sample{name: "POST /v1/templates 422 validation", method: "POST", path: "/v1/templates",
		body: `{"name":"bad","channel":"sms","body":"Hi {{name"}`, want: 422})
	run(t, sample{name: "POST /v1/templates 400 malformed json", method: "POST", path: "/v1/templates",
		body: `[`, want: 400, badRequest: true})
	run(t, sample{name: "POST /v1/templates 413 oversize", method: "POST", path: "/v1/templates",
		body: `{"name":"big","channel":"sms","body":"` + strings.Repeat("a", 70*1024) + `"}`, want: 413, badRequest: true})
	run(t, sample{name: "GET /v1/templates/{id} 200", method: "GET", path: "/v1/templates/" + tplID, want: 200})
	run(t, sample{name: "GET /v1/templates/{id} 404", method: "GET", path: "/v1/templates/123e4567-e89b-12d3-a456-426614174000", want: 404})
	run(t, sample{name: "GET /v1/templates 200", method: "GET", path: "/v1/templates?limit=10", want: 200})
	run(t, sample{name: "GET /v1/templates 422 bad limit", method: "GET", path: "/v1/templates?limit=0", want: 422, badRequest: true})
	tn := run(t, sample{name: "POST /v1/notifications 201 from template", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"+905551234567","channel":"sms","template_id":"` + tplID + `","template_vars":{"name":"Ada"}}`, want: 201})
	if tn["template_id"] != tplID || tn["content"] != "Hi Ada" {
		t.Fatalf("template notification: %v", tn)
	}
	run(t, sample{name: "POST /v1/notifications 422 unknown template", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"+905551234567","channel":"sms","template_id":"123e4567-e89b-12d3-a456-426614174000"}`, want: 422})

	// Get, with a finished and an unfinished delivery attempt.
	if _, err := pool.Exec(t.Context(), `INSERT INTO notification_attempts (notification_id, attempt_no, started_at, finished_at, http_status, error, duration_ms)
		VALUES ($1::uuid, 1, now(), now(), 500, 'provider 500', 12), ($1::uuid, 2, now(), NULL, NULL, NULL, NULL)`, id); err != nil {
		t.Fatal(err)
	}
	d := run(t, sample{name: "GET /v1/notifications/{id} 200", method: "GET", path: "/v1/notifications/" + id, want: 200})
	if as, _ := d["delivery_attempts"].([]any); len(as) != 2 {
		t.Fatalf("attempts: %v", d["delivery_attempts"])
	}
	run(t, sample{name: "GET /v1/notifications/{id} 404", method: "GET", path: "/v1/notifications/123e4567-e89b-12d3-a456-426614174000", want: 404})
	run(t, sample{name: "GET /v1/notifications/{id} 404 malformed id", method: "GET", path: "/v1/notifications/not-a-uuid", want: 404, badRequest: true})

	// Cancel and retry.
	c := run(t, sample{name: "POST /v1/notifications 201 to cancel", method: "POST", path: "/v1/notifications",
		body: `{"recipient":"+905551234567","channel":"sms","content":"cancel me"}`, want: 201})
	cid := str(t, c, "id")
	run(t, sample{name: "POST /v1/notifications/{id}/cancel 200", method: "POST", path: "/v1/notifications/" + cid + "/cancel", want: 200})
	run(t, sample{name: "POST /v1/notifications/{id}/cancel 409", method: "POST", path: "/v1/notifications/" + cid + "/cancel", want: 409})
	run(t, sample{name: "POST /v1/notifications/{id}/cancel 404", method: "POST", path: "/v1/notifications/123e4567-e89b-12d3-a456-426614174000/cancel", want: 404})
	run(t, sample{name: "POST /v1/notifications/{id}/retry 409", method: "POST", path: "/v1/notifications/" + id + "/retry", want: 409})
	if _, err := pool.Exec(t.Context(), `UPDATE notifications SET status='failed', attempts=8, last_error='provider 500' WHERE id=$1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	r := run(t, sample{name: "POST /v1/notifications/{id}/retry 200", method: "POST", path: "/v1/notifications/" + id + "/retry", want: 200})
	if r["status"] != "pending" || r["last_error"] != "provider 500" {
		t.Fatalf("retried: %v", r)
	}
	run(t, sample{name: "POST /v1/notifications/{id}/retry 404", method: "POST", path: "/v1/notifications/123e4567-e89b-12d3-a456-426614174000/retry", want: 404})

	// List: filter, cursor page, bad limit.
	run(t, sample{name: "GET /v1/notifications 200 filtered", method: "GET", path: "/v1/notifications?status=pending&channel=sms&limit=50", want: 200})
	p1 := run(t, sample{name: "GET /v1/notifications 200 first page", method: "GET", path: "/v1/notifications?limit=2", want: 200})
	cur := str(t, p1, "next_cursor")
	run(t, sample{name: "GET /v1/notifications 200 second page", method: "GET", path: "/v1/notifications?limit=2&cursor=" + url.QueryEscape(cur), want: 200})
	run(t, sample{name: "GET /v1/notifications 200 last page", method: "GET", path: "/v1/notifications?limit=100", want: 200})
	run(t, sample{name: "GET /v1/notifications 400 bad limit", method: "GET", path: "/v1/notifications?limit=abc", want: 400, badRequest: true})
	run(t, sample{name: "GET /v1/notifications 400 bad cursor", method: "GET", path: "/v1/notifications?cursor=garbage", want: 400})

	// Batches.
	bk := map[string]string{"Idempotency-Key": "contract-batch-1"}
	batchBody := `{"notifications":[` + sms + `,{"recipient":"a@example.com","channel":"email","content":"hi","scheduled_at":"` + future + `"}]}`
	b := run(t, sample{name: "POST /v1/notifications/batch 201", method: "POST", path: "/v1/notifications/batch", body: batchBody, header: bk, want: 201})
	bid := str(t, b, "batch_id")
	run(t, sample{name: "POST /v1/notifications/batch 200 replay", method: "POST", path: "/v1/notifications/batch", body: batchBody, header: bk, want: 200})
	run(t, sample{name: "POST /v1/notifications/batch 409 idempotency conflict", method: "POST", path: "/v1/notifications/batch",
		body: `{"notifications":[` + sms + `]}`, header: bk, want: 409})
	run(t, sample{name: "POST /v1/notifications/batch 422 empty", method: "POST", path: "/v1/notifications/batch", body: `{"notifications":[]}`, want: 422, badRequest: true})
	run(t, sample{name: "POST /v1/notifications/batch 422 item errors", method: "POST", path: "/v1/notifications/batch",
		body: `{"notifications":[` + sms + `,{"recipient":"bad","channel":"sms","content":"x"}]}`, want: 422})
	run(t, sample{name: "POST /v1/notifications/batch 400 malformed json", method: "POST", path: "/v1/notifications/batch", body: `{`, want: 400, badRequest: true})
	run(t, sample{name: "POST /v1/notifications/batch 413 oversize", method: "POST", path: "/v1/notifications/batch",
		body: `{"notifications":[{"recipient":"+905551234567","channel":"sms","content":"` + strings.Repeat("a", 9<<20) + `"}]}`, want: 413, badRequest: true})
	run(t, sample{name: "GET /v1/notifications 200 by batch", method: "GET", path: "/v1/notifications?batch_id=" + bid, want: 200})
	run(t, sample{name: "GET /v1/batches/{id} 200", method: "GET", path: "/v1/batches/" + bid, want: 200})
	run(t, sample{name: "GET /v1/batches/{id} 404", method: "GET", path: "/v1/batches/123e4567-e89b-12d3-a456-426614174000", want: 404})
	cb := run(t, sample{name: "POST /v1/batches/{id}/cancel 200", method: "POST", path: "/v1/batches/" + bid + "/cancel", want: 200})
	if cb["cancelled"] != float64(2) {
		t.Fatalf("cancelled: %v", cb)
	}
	run(t, sample{name: "POST /v1/batches/{id}/cancel 404", method: "POST", path: "/v1/batches/123e4567-e89b-12d3-a456-426614174000/cancel", want: 404})
}
