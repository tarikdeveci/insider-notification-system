package mockprovider

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, cfg Config, rnd func() float64) (*Server, *httptest.Server) {
	t.Helper()
	s := New(Options{Config: cfg, Rand: rnd, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func defaultCfg() Config {
	return Config{FailureStatus: 503, RetryAfterS: 1}
}

func deliver(t *testing.T, base, key, to, channel string) (*http.Response, map[string]any) {
	t.Helper()
	body := `{"to":"` + to + `","channel":"` + channel + `","content":"hello"}`
	req, err := http.NewRequest(http.MethodPost, base+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return doJSON(t, req)
}

func doJSON(t *testing.T, req *http.Request) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func control(t *testing.T, method, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	return doJSON(t, req)
}

type gotArrival struct {
	Key          string `json:"idempotency_key"`
	To           string `json:"to"`
	Channel      string `json:"channel"`
	Status       int    `json:"status"`
	Deduplicated bool   `json:"deduplicated"`
	At           string `json:"at"`
}

func arrivals(t *testing.T, base, query string) ([]gotArrival, int) {
	t.Helper()
	resp, err := http.Get(base + "/_control/arrivals" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Arrivals  []gotArrival `json:"arrivals"`
		Delivered int          `json:"delivered"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Arrivals, out.Delivered
}

func TestReturns202WithMessageId(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	resp, body := deliver(t, ts.URL, "k1", "a@x.io", "email")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	id, _ := body["messageId"].(string)
	if id == "" || body["status"] != "accepted" {
		t.Fatalf("body = %v", body)
	}
}

func TestRejectsMissingKeyAndBadBody(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	if resp, _ := deliver(t, ts.URL, "", "a", "sms"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing key status = %d", resp.StatusCode)
	}
	for _, body := range []string{"", "null", "[]", `{"to":"a"`, `{"channel":"sms"}`} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "k")
		if resp, _ := doJSON(t, req); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q status = %d", body, resp.StatusCode)
		}
	}
}

func TestDedupsByIdempotencyKey(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	_, first := deliver(t, ts.URL, "dup", "a", "sms")
	resp, second := deliver(t, ts.URL, "dup", "a", "sms")
	if resp.StatusCode != http.StatusAccepted || first["messageId"] != second["messageId"] {
		t.Fatalf("dedup failed: %v vs %v (%d)", first, second, resp.StatusCode)
	}
	_, other := deliver(t, ts.URL, "other", "a", "sms")
	if other["messageId"] == first["messageId"] {
		t.Fatal("distinct keys share a messageId")
	}
	list, delivered := arrivals(t, ts.URL, "")
	if delivered != 2 || len(list) != 3 {
		t.Fatalf("delivered=%d arrivals=%d", delivered, len(list))
	}
	if list[0].Deduplicated || !list[1].Deduplicated {
		t.Fatalf("dedup flags wrong: %+v", list)
	}
}

func TestConfigurableFailureRateLatencyAndStatus(t *testing.T) {
	rnd := 0.0
	_, ts := newTestServer(t, defaultCfg(), func() float64 { return rnd })

	resp, cfg := control(t, http.MethodGet, ts.URL+"/_control/config", "")
	if resp.StatusCode != 200 || cfg["failure_status"] != float64(503) {
		t.Fatalf("get config: %d %v", resp.StatusCode, cfg)
	}
	put := `{"failure_rate":0.5,"failure_status":500,"latency_ms":60,"throttle":false,"retry_after_s":1}`
	if resp, _ := control(t, http.MethodPut, ts.URL+"/_control/config", put); resp.StatusCode != 200 {
		t.Fatalf("put config: %d", resp.StatusCode)
	}
	start := time.Now()
	resp, _ = deliver(t, ts.URL, "f1", "a", "sms") // rand 0.0 < 0.5: fails
	if resp.StatusCode != 500 {
		t.Fatalf("failure status = %d", resp.StatusCode)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("latency not applied")
	}
	rnd = 0.9 // above the rate: succeeds
	if resp, _ = deliver(t, ts.URL, "f2", "a", "sms"); resp.StatusCode != 202 {
		t.Fatalf("success status = %d", resp.StatusCode)
	}
}

func TestRecordsArrivalTimestamps(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	before := time.Now().Add(-time.Second)
	deliver(t, ts.URL, "t1", "a@x.io", "email")
	deliver(t, ts.URL, "t2", "b", "sms")
	list, _ := arrivals(t, ts.URL, "")
	if len(list) != 2 {
		t.Fatalf("arrivals = %d", len(list))
	}
	var prev time.Time
	for _, a := range list {
		at, err := time.Parse(time.RFC3339Nano, a.At)
		if err != nil || at.Before(before) || at.Before(prev) {
			t.Fatalf("bad timestamp %q (%v)", a.At, err)
		}
		prev = at
	}
	if list[0].Key != "t1" || list[1].Key != "t2" {
		t.Fatalf("order: %+v", list)
	}
	if only, _ := arrivals(t, ts.URL, "?channel=sms"); len(only) != 1 || only[0].To != "b" {
		t.Fatalf("channel filter: %+v", only)
	}
	if only, _ := arrivals(t, ts.URL, "?to=a@x.io"); len(only) != 1 || only[0].Key != "t1" {
		t.Fatalf("to filter: %+v", only)
	}
}

func TestThrottleMode(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	put := `{"failure_rate":0,"failure_status":503,"latency_ms":0,"throttle":true,"retry_after_s":7}`
	control(t, http.MethodPut, ts.URL+"/_control/config", put)
	resp, _ := deliver(t, ts.URL, "th", "a", "sms")
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("throttle: %d retry-after=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestScriptPerRecipientThenDefault(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	resp, _ := control(t, http.MethodPost, ts.URL+"/_control/script", `{"to":"scripted","statuses":[503,429,202]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("script: %d", resp.StatusCode)
	}
	var got []int
	for i := 0; i < 4; i++ {
		r, _ := deliver(t, ts.URL, "s", "scripted", "sms")
		got = append(got, r.StatusCode)
	}
	want := []int{503, 429, 202, 202} // empty script falls back to default rules
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	// other recipients are unaffected by the script.
	if r, _ := deliver(t, ts.URL, "o", "someone-else", "sms"); r.StatusCode != 202 {
		t.Fatalf("unscripted status = %d", r.StatusCode)
	}
}

func TestArrivalsIncludeFailedAndDeduplicated(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	control(t, http.MethodPost, ts.URL+"/_control/script", `{"to":"r","statuses":[503]}`)
	deliver(t, ts.URL, "k", "r", "email") // 503
	deliver(t, ts.URL, "k", "r", "email") // 202
	deliver(t, ts.URL, "k", "r", "email") // deduplicated 202
	list, delivered := arrivals(t, ts.URL, "")
	if len(list) != 3 || delivered != 1 {
		t.Fatalf("arrivals=%d delivered=%d", len(list), delivered)
	}
	if list[0].Status != 503 || list[0].Deduplicated {
		t.Fatalf("failed arrival: %+v", list[0])
	}
	if list[1].Status != 202 || list[1].Deduplicated {
		t.Fatalf("first success: %+v", list[1])
	}
	if list[2].Status != 202 || !list[2].Deduplicated {
		t.Fatalf("dedup arrival: %+v", list[2])
	}
}

func TestResetClearsState(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	control(t, http.MethodPost, ts.URL+"/_control/script", `{"to":"r","statuses":[500]}`)
	control(t, http.MethodPut, ts.URL+"/_control/config", `{"failure_rate":0,"failure_status":500,"latency_ms":0,"throttle":true,"retry_after_s":3}`)
	control(t, http.MethodPut, ts.URL+"/_control/config", `{"failure_rate":0,"failure_status":503,"latency_ms":0,"throttle":false,"retry_after_s":1}`)
	deliver(t, ts.URL, "k", "x", "sms")
	control(t, http.MethodPut, ts.URL+"/_control/config", `{"failure_rate":0,"failure_status":500,"latency_ms":0,"throttle":true,"retry_after_s":3}`)
	if resp, _ := control(t, http.MethodPost, ts.URL+"/_control/reset", ""); resp.StatusCode != 200 {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	list, delivered := arrivals(t, ts.URL, "")
	if len(list) != 0 || delivered != 0 {
		t.Fatalf("arrivals survived reset: %d %d", len(list), delivered)
	}
	_, cfg := control(t, http.MethodGet, ts.URL+"/_control/config", "")
	if cfg["throttle"] != false || cfg["failure_status"] != float64(503) {
		t.Fatalf("config not restored: %v", cfg)
	}
	// dedup map cleared: same key is a fresh delivery; script cleared: recipient r succeeds.
	deliver(t, ts.URL, "k", "x", "sms")
	if r, _ := deliver(t, ts.URL, "k2", "r", "sms"); r.StatusCode != 202 {
		t.Fatalf("script survived reset: %d", r.StatusCode)
	}
	if _, delivered := arrivals(t, ts.URL, ""); delivered != 2 {
		t.Fatalf("delivered after reset = %d", delivered)
	}
}

func TestInvalidControlConfigIs400(t *testing.T) {
	_, ts := newTestServer(t, defaultCfg(), nil)
	bad := []string{
		`{"failure_rate":1.5,"failure_status":503,"latency_ms":0,"retry_after_s":1}`,
		`{"failure_rate":-0.1,"failure_status":503,"latency_ms":0,"retry_after_s":1}`,
		`{"failure_rate":0,"failure_status":99,"latency_ms":0,"retry_after_s":1}`,
		`{"failure_rate":0,"failure_status":600,"latency_ms":0,"retry_after_s":1}`,
		`{"failure_rate":0,"failure_status":503,"latency_ms":60001,"retry_after_s":1}`,
		`{"failure_rate":0,"failure_status":503,"latency_ms":-1,"retry_after_s":1}`,
		`{"failure_rate":0,"failure_status":503,"latency_ms":0,"retry_after_s":3601}`,
		`{"failure_rate":0,"failure_status":503,"latency_ms":0,"retry_after_s":-1}`,
		`not json`,
		`{"failure_rate":0,"bogus":1}`,
	}
	for _, b := range bad {
		if resp, _ := control(t, http.MethodPut, ts.URL+"/_control/config", b); resp.StatusCode != 400 {
			t.Fatalf("%s: status = %d", b, resp.StatusCode)
		}
	}
	for _, b := range []string{`{"to":"","statuses":[202]}`, `{"to":"a","statuses":[]}`, `{"to":"a","statuses":[42]}`} {
		if resp, _ := control(t, http.MethodPost, ts.URL+"/_control/script", b); resp.StatusCode != 400 {
			t.Fatalf("script %s: status = %d", b, resp.StatusCode)
		}
	}
	// config unchanged after rejected updates.
	if _, cfg := control(t, http.MethodGet, ts.URL+"/_control/config", ""); cfg["failure_status"] != float64(503) {
		t.Fatalf("config mutated: %v", cfg)
	}
}
