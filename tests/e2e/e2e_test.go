//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var base = envOr("E2E_BASE_URL", "http://127.0.0.1:8080")

// client bounds every e2e request; never use http.DefaultClient or bare http.Get.
var client = &http.Client{Timeout: 10 * time.Second}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func TestMain(m *testing.M) {
	probe := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := probe.Get(base + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				os.Exit(m.Run())
			}
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintln(os.Stderr, "api not ready within 90s")
	os.Exit(1)
}

func uniq() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func call(t *testing.T, method, path, body string, hdr map[string]string) (int, http.Header, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.NewDecoder(bytes.NewReader(raw)).Decode(&m)
	return resp.StatusCode, resp.Header, m
}

func sms(content string) string {
	return fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","content":%q}`, content)
}

func TestCreateGetListCancel(t *testing.T) {
	code, _, n := call(t, "POST", "/v1/notifications", sms("e2e "+uniq()), nil)
	if code != 201 || n["status"] != "pending" {
		t.Fatalf("create: %d %v", code, n)
	}
	id := n["id"].(string)
	if code, _, got := call(t, "GET", "/v1/notifications/"+id, "", nil); code != 200 || got["id"] != id {
		t.Fatalf("get: %d %v", code, got)
	}
	code, _, list := call(t, "GET", "/v1/notifications?status=pending&channel=sms&limit=5", "", nil)
	if code != 200 || len(list["items"].([]any)) == 0 {
		t.Fatalf("list: %d %v", code, list)
	}
	if code, _, c := call(t, "POST", "/v1/notifications/"+id+"/cancel", "", nil); code != 200 || c["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", code, c)
	}
	if code, _, _ := call(t, "POST", "/v1/notifications/"+id+"/cancel", "", nil); code != 409 {
		t.Fatalf("second cancel: %d", code)
	}
}

func TestIdempotentReplayAndConflict(t *testing.T) {
	key := map[string]string{"Idempotency-Key": "e2e-" + uniq()}
	body := sms("idem " + uniq())
	c1, _, a := call(t, "POST", "/v1/notifications", body, key)
	c2, _, b := call(t, "POST", "/v1/notifications", body, key)
	if c1 != 201 || c2 != 200 || a["id"] != b["id"] {
		t.Fatalf("replay: %d %d %v %v", c1, c2, a, b)
	}
	if c3, _, _ := call(t, "POST", "/v1/notifications", sms("other"), key); c3 != 409 {
		t.Fatalf("conflict: %d", c3)
	}
}

func TestBatchLifecycle(t *testing.T) {
	key := map[string]string{"Idempotency-Key": "e2e-b-" + uniq()}
	body := `{"notifications":[` + sms("b0") + `,` + sms("b1") + `,` + sms("b2") + `]}`
	code, _, created := call(t, "POST", "/v1/notifications/batch", body, key)
	if code != 201 {
		t.Fatalf("create: %d %v", code, created)
	}
	bid := created["batch_id"].(string)
	code, _, replay := call(t, "POST", "/v1/notifications/batch", body, key)
	if code != 200 || replay["batch_id"] != bid || fmt.Sprint(replay["notification_ids"]) != fmt.Sprint(created["notification_ids"]) {
		t.Fatalf("replay: %d %v", code, replay)
	}
	_, _, sum := call(t, "GET", "/v1/batches/"+bid, "", nil)
	if sum["counts"].(map[string]any)["pending"].(float64) != 3 {
		t.Fatalf("summary: %v", sum)
	}
	_, _, cancelled := call(t, "POST", "/v1/batches/"+bid+"/cancel", "", nil)
	if cancelled["cancelled"].(float64) != 3 {
		t.Fatalf("cancel: %v", cancelled)
	}
	_, _, sum = call(t, "GET", "/v1/batches/"+bid, "", nil)
	if sum["counts"].(map[string]any)["cancelled"].(float64) != 3 {
		t.Fatalf("after cancel: %v", sum)
	}
}

func TestValidationIsProblemJSON(t *testing.T) {
	code, hdr, m := call(t, "POST", "/v1/notifications", `{"recipient":"x","channel":"sms","content":""}`, nil)
	if code != 422 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/problem+json") || len(m["errors"].([]any)) < 2 {
		t.Fatalf("%d %s %v", code, hdr.Get("Content-Type"), m)
	}
}

func TestRequestIDEcho(t *testing.T) {
	_, hdr, _ := call(t, "GET", "/healthz", "", map[string]string{"X-Request-ID": "e2e-id-1"})
	if hdr.Get("X-Request-ID") != "e2e-id-1" {
		t.Fatalf("got %q", hdr.Get("X-Request-ID"))
	}
}
