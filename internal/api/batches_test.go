package api_test

import (
	"fmt"
	"strings"
	"testing"
)

func batchJSON(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","content":"m%d"}`, i)
	}
	return `{"notifications":[` + strings.Join(items, ",") + `]}`
}

func TestBatchCreateOrderAndReplay(t *testing.T) {
	h := newAPI(t)
	key := map[string]string{"Idempotency-Key": "b1"}
	first := call(t, h, "POST", "/v1/notifications/batch", batchJSON(3), key)
	if first.Code != 201 {
		t.Fatalf("code %d: %s", first.Code, first.Body)
	}
	m := obj(t, first)
	ids := m["notification_ids"].([]any)
	if m["batch_id"] == "" || len(ids) != 3 {
		t.Fatalf("body: %v", m)
	}
	for i, id := range ids {
		n := obj(t, call(t, h, "GET", "/v1/notifications/"+id.(string), "", nil))
		if n["content"] != fmt.Sprintf("m%d", i) || n["batch_id"] != m["batch_id"] {
			t.Fatalf("item %d: %v", i, n)
		}
	}
	again := call(t, h, "POST", "/v1/notifications/batch", batchJSON(3), key)
	m2 := obj(t, again)
	if again.Code != 200 || m2["batch_id"] != m["batch_id"] || fmt.Sprint(m2["notification_ids"]) != fmt.Sprint(m["notification_ids"]) {
		t.Fatalf("replay: %d %v", again.Code, m2)
	}
	if rr := call(t, h, "POST", "/v1/notifications/batch", batchJSON(2), key); rr.Code != 409 {
		t.Fatalf("changed payload: %d", rr.Code)
	}
}

func TestBatchValidationIsAllOrNothing(t *testing.T) {
	h := newAPI(t)
	body := `{"notifications":[{"recipient":"+905551234567","channel":"sms","content":"ok"},{"recipient":"bad","channel":"sms","content":"x"}]}`
	rr := call(t, h, "POST", "/v1/notifications/batch", body, nil)
	if rr.Code != 422 {
		t.Fatalf("code %d", rr.Code)
	}
	first := obj(t, rr)["errors"].([]any)[0].(map[string]any)
	if first["index"].(float64) != 1 || first["field"] != "recipient" {
		t.Fatalf("error: %v", first)
	}
	if n := len(obj(t, call(t, h, "GET", "/v1/notifications", "", nil))["items"].([]any)); n != 0 {
		t.Fatalf("nothing may be inserted, found %d", n)
	}
}

func TestBatchSizeLimits(t *testing.T) {
	h := newAPI(t)
	if rr := call(t, h, "POST", "/v1/notifications/batch", `{"notifications":[]}`, nil); rr.Code != 422 {
		t.Errorf("empty: %d", rr.Code)
	}
	if rr := call(t, h, "POST", "/v1/notifications/batch", batchJSON(1001), nil); rr.Code != 422 {
		t.Errorf("1001: %d", rr.Code)
	}
	if rr := call(t, h, "POST", "/v1/notifications/batch", batchJSON(1000), nil); rr.Code != 201 {
		t.Errorf("1000: %d %.200s", rr.Code, rr.Body)
	}
	for name, body := range map[string]string{"null": `null`, "array": `[]`, "empty": ``, "unknown": `{"items":[]}`} {
		if rr := call(t, h, "POST", "/v1/notifications/batch", body, nil); rr.Code != 400 {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
}

func TestBatchRejectsBadIdempotencyKey(t *testing.T) {
	h := newAPI(t)
	for _, k := range []string{"has space", strings.Repeat("k", 129), "café"} {
		if rr := call(t, h, "POST", "/v1/notifications/batch", batchJSON(1), map[string]string{"Idempotency-Key": k}); rr.Code != 400 {
			t.Errorf("key %q: %d", k, rr.Code)
		}
	}
}

func TestBatchSummaryAndCancel(t *testing.T) {
	h := newAPI(t)
	m := obj(t, call(t, h, "POST", "/v1/notifications/batch", batchJSON(3), nil))
	bid := m["batch_id"].(string)
	sum := obj(t, call(t, h, "GET", "/v1/batches/"+bid, "", nil))
	if sum["total"].(float64) != 3 || sum["counts"].(map[string]any)["pending"].(float64) != 3 {
		t.Fatalf("summary: %v", sum)
	}
	one := m["notification_ids"].([]any)[0].(string)
	if rr := call(t, h, "POST", "/v1/notifications/"+one+"/cancel", "", nil); rr.Code != 200 || obj(t, rr)["status"] != "cancelled" {
		t.Fatalf("single cancel: %d", rr.Code)
	}
	if rr := call(t, h, "POST", "/v1/notifications/"+one+"/cancel", "", nil); rr.Code != 409 {
		t.Fatalf("second cancel: %d", rr.Code)
	}
	rr := call(t, h, "POST", "/v1/batches/"+bid+"/cancel", "", nil)
	if rr.Code != 200 || obj(t, rr)["cancelled"].(float64) != 2 {
		t.Fatalf("batch cancel: %d %s", rr.Code, rr.Body)
	}
	rr = call(t, h, "POST", "/v1/batches/"+bid+"/cancel", "", nil)
	if rr.Code != 200 || obj(t, rr)["cancelled"].(float64) != 0 {
		t.Fatalf("repeat batch cancel: %d %s", rr.Code, rr.Body)
	}
}

func TestCancelAndBatchMissingOrMalformed(t *testing.T) {
	h := newAPI(t)
	missing := "123e4567-e89b-12d3-a456-426614174000"
	for _, p := range []string{
		"/v1/notifications/" + missing + "/cancel", "/v1/batches/" + missing + "/cancel",
		"/v1/notifications/not-a-uuid/cancel", "/v1/batches/not-a-uuid/cancel",
	} {
		if rr := call(t, h, "POST", p, "", nil); rr.Code != 404 {
			t.Errorf("POST %s: %d", p, rr.Code)
		}
	}
	for _, p := range []string{"/v1/batches/" + missing, "/v1/batches/not-a-uuid"} {
		if rr := call(t, h, "GET", p, "", nil); rr.Code != 404 {
			t.Errorf("GET %s: %d", p, rr.Code)
		}
	}
}
