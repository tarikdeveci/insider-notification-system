package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func newAPI(t *testing.T) http.Handler {
	t.Helper()
	pool := testutil.MigratedPool(t)
	return api.NewServer(api.Deps{Store: store.New(pool), DB: pool, Redis: fakePinger{}, Logger: discardLogger()})
}

func call(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func obj(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, rr.Body)
	}
	return m
}

const smsBody = `{"recipient":"+905551234567","channel":"sms","content":"hello"}`

func TestCreateNotification(t *testing.T) {
	h := newAPI(t)
	rr := call(t, h, "POST", "/v1/notifications", smsBody, nil)
	if rr.Code != 201 {
		t.Fatalf("code %d: %s", rr.Code, rr.Body)
	}
	m := obj(t, rr)
	if m["id"] == "" || m["status"] != "pending" || m["priority"] != "normal" || m["channel"] != "sms" || m["batch_id"] != nil {
		t.Fatalf("body: %v", m)
	}
	if rr.Header().Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID")
	}
}

func TestCorrelationIDIsStored(t *testing.T) {
	h := newAPI(t)
	rr := call(t, h, "POST", "/v1/notifications", smsBody, map[string]string{"X-Request-ID": "req-abc"})
	id := obj(t, rr)["id"].(string)
	got := obj(t, call(t, h, "GET", "/v1/notifications/"+id, "", nil))
	if got["correlation_id"] != "req-abc" {
		t.Fatalf("correlation_id = %v", got["correlation_id"])
	}
	if atts, ok := got["delivery_attempts"].([]any); !ok || len(atts) != 0 {
		t.Fatalf("delivery_attempts must be an empty array: %v", got["delivery_attempts"])
	}
}

func TestCreateValidation422(t *testing.T) {
	h := newAPI(t)
	body := `{"recipient":"123","channel":"sms","content":"` + strings.Repeat("a", 161) + `"}`
	rr := call(t, h, "POST", "/v1/notifications", body, nil)
	if rr.Code != 422 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("code=%d ct=%s", rr.Code, rr.Header().Get("Content-Type"))
	}
	fields := map[string]bool{}
	for _, e := range obj(t, rr)["errors"].([]any) {
		fields[e.(map[string]any)["field"].(string)] = true
	}
	if !fields["recipient"] || !fields["content"] {
		t.Fatalf("errors: %v", fields)
	}
}

func TestCreateUnicodeSMSLimitIsInRunes(t *testing.T) {
	h := newAPI(t)
	ok := `{"recipient":"+905551234567","channel":"sms","content":"` + strings.Repeat("ş", 160) + `"}`
	if rr := call(t, h, "POST", "/v1/notifications", ok, nil); rr.Code != 201 {
		t.Fatalf("160 runes: %d %s", rr.Code, rr.Body)
	}
	bad := `{"recipient":"+905551234567","channel":"sms","content":"` + strings.Repeat("ş", 161) + `"}`
	if rr := call(t, h, "POST", "/v1/notifications", bad, nil); rr.Code != 422 {
		t.Fatalf("161 runes: %d", rr.Code)
	}
}

func TestCreateBadBodies(t *testing.T) {
	h := newAPI(t)
	for name, body := range map[string]string{
		"empty": ``, "null": `null`, "array": `[]`, "unknown field": `{"recipient":"+905551234567","channel":"sms","content":"x","bogus":"t"}`,
		"truncated": `{"recipient"`, "trailing": smsBody + smsBody,
	} {
		rr := call(t, h, "POST", "/v1/notifications", body, nil)
		if rr.Code != 400 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: code=%d", name, rr.Code)
		}
	}
	big := `{"recipient":"+905551234567","channel":"sms","content":"` + strings.Repeat("a", 70<<10) + `"}`
	if rr := call(t, h, "POST", "/v1/notifications", big, nil); rr.Code != 413 {
		t.Errorf("oversize: %d", rr.Code)
	}
}

func TestCreateIdempotency(t *testing.T) {
	h := newAPI(t)
	key := map[string]string{"Idempotency-Key": "key-1"}
	first := call(t, h, "POST", "/v1/notifications", smsBody, key)
	if first.Code != 201 {
		t.Fatalf("first: %d", first.Code)
	}
	again := call(t, h, "POST", "/v1/notifications", smsBody, key)
	if again.Code != 200 || obj(t, again)["id"] != obj(t, first)["id"] {
		t.Fatalf("replay: %d %s", again.Code, again.Body)
	}
	explicit := `{"recipient":"+905551234567","channel":"sms","content":"hello","priority":"normal"}`
	if rr := call(t, h, "POST", "/v1/notifications", explicit, key); rr.Code != 200 {
		t.Fatalf("omitted priority vs normal must replay: %d", rr.Code)
	}
	other := `{"recipient":"+905551234567","channel":"sms","content":"different"}`
	if rr := call(t, h, "POST", "/v1/notifications", other, key); rr.Code != 409 {
		t.Fatalf("changed payload: %d", rr.Code)
	}
}

func TestCreateBadIdempotencyKeys(t *testing.T) {
	h := newAPI(t)
	for name, k := range map[string]string{"space": "a b", "129 chars": strings.Repeat("k", 129), "non ascii": "é"} {
		rr := call(t, h, "POST", "/v1/notifications", smsBody, map[string]string{"Idempotency-Key": k})
		if rr.Code != 400 {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	// Empty and repeated headers cannot go through the map helper.
	for name, vals := range map[string][]string{"empty": {""}, "repeated": {"a", "b"}} {
		r := httptest.NewRequest("POST", "/v1/notifications", strings.NewReader(smsBody))
		r.Header["Idempotency-Key"] = vals
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != 400 {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	if rr := call(t, h, "POST", "/v1/notifications", smsBody, map[string]string{"Idempotency-Key": strings.Repeat("k", 128)}); rr.Code != 201 {
		t.Errorf("128 chars must pass: %d", rr.Code)
	}
}

func TestGetNotFoundAndMalformedID(t *testing.T) {
	h := newAPI(t)
	for _, id := range []string{"123e4567-e89b-12d3-a456-426614174000", "not-a-uuid"} {
		rr := call(t, h, "GET", "/v1/notifications/"+id, "", nil)
		if rr.Code != 404 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: %d", id, rr.Code)
		}
	}
}

func TestListFiltersAndPagination(t *testing.T) {
	h := newAPI(t)
	for i := 0; i < 3; i++ {
		call(t, h, "POST", "/v1/notifications", smsBody, nil)
	}
	call(t, h, "POST", "/v1/notifications", `{"recipient":"a@example.com","channel":"email","content":"x"}`, nil)

	page1 := obj(t, call(t, h, "GET", "/v1/notifications?channel=sms&limit=2", "", nil))
	items := page1["items"].([]any)
	cursor, _ := page1["next_cursor"].(string)
	if len(items) != 2 || cursor == "" {
		t.Fatalf("page1: %v", page1)
	}
	page2 := obj(t, call(t, h, "GET", "/v1/notifications?channel=sms&limit=2&cursor="+cursor, "", nil))
	if len(page2["items"].([]any)) != 1 || page2["next_cursor"] != nil {
		t.Fatalf("page2: %v", page2)
	}
	if n := len(obj(t, call(t, h, "GET", "/v1/notifications?status=cancelled", "", nil))["items"].([]any)); n != 0 {
		t.Errorf("status filter: %d", n)
	}
}

func TestListBadParams(t *testing.T) {
	h := newAPI(t)
	for _, q := range []string{
		"limit=0", "limit=101", "limit=abc", "status=bogus", "channel=fax", "cursor=garbage", "batch_id=x",
		"created_from=yesterday", "created_from=2026-02-01T00:00:00Z&created_to=2026-01-01T00:00:00Z",
	} {
		rr := call(t, h, "GET", "/v1/notifications?"+q, "", nil)
		if rr.Code != 400 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: %d", q, rr.Code)
		}
	}
	rr := call(t, h, "GET", "/v1/notifications?limit=abc&status=bogus", "", nil)
	fields := map[string]bool{}
	for _, e := range obj(t, rr)["errors"].([]any) {
		fields[e.(map[string]any)["field"].(string)] = true
	}
	if !fields["limit"] || !fields["status"] {
		t.Errorf("each bad parameter must be listed: %v", fields)
	}
}
