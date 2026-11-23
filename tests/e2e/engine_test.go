//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// Engine phase tests run with the worker up (E2E_PHASE=engine); see the Makefile e2e target.

var mockBase = envOr("E2E_MOCK_URL", "http://127.0.0.1:9090")

const (
	pollEvery       = 100 * time.Millisecond
	defaultDeadline = 30 * time.Second
)

func needEnginePhase(t *testing.T) {
	t.Helper()
	if !enginePhase() {
		t.Skip("engine test: runs when E2E_PHASE=engine")
	}
}

// engineReady proves the worker is consuming: one sms must reach sent within 90s.
// Every request uses a 2s client (PF4).
func engineReady() error {
	probe := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	body := fmt.Sprintf(`{"recipient":%q,"channel":"sms","content":"canary"}`, smsRecipient())
	var id string
	for time.Now().Before(deadline) && id == "" {
		resp, err := probe.Post(base+"/v1/notifications", "application/json", strings.NewReader(body))
		if err == nil {
			var n struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&n)
			resp.Body.Close()
			if resp.StatusCode == http.StatusCreated {
				id = n.ID
			}
		}
		if id == "" {
			time.Sleep(time.Second)
		}
	}
	for id != "" && time.Now().Before(deadline) {
		resp, err := probe.Get(base + "/v1/notifications/" + id)
		if err == nil {
			var n struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&n)
			resp.Body.Close()
			if n.Status == "sent" {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return errors.New("canary sms not sent within 90s")
}

// smsRecipient returns a unique valid E.164 number.
func smsRecipient() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("+9%012d", n)
}

func emailRecipient() string { return "e2e-" + uniq() + "@example.com" }

type attempt struct {
	AttemptNo  int  `json:"attempt_no"`
	HTTPStatus *int `json:"http_status"`
}

type notif struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError *string   `json:"last_error"`
	Delivery  []attempt `json:"delivery_attempts"`
}

func getNotif(t *testing.T, id string) notif {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/notifications/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var n notif
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&n) != nil {
		t.Fatalf("get %s: status %d", id, resp.StatusCode)
	}
	return n
}

// waitStatus polls every 100ms until the notification reaches one of want.
func waitStatus(t *testing.T, id string, d time.Duration, want ...string) notif {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		n := getNotif(t, id)
		for _, w := range want {
			if n.Status == w {
				return n
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("notification %s: status %q (attempts %d), want %v within %s", id, n.Status, n.Attempts, want, d)
		}
		time.Sleep(pollEvery)
	}
}

func mockDo(t *testing.T, method, path, body string) []byte {
	t.Helper()
	req, err := http.NewRequest(method, mockBase+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("mock %s %s: status %d err %v", method, path, resp.StatusCode, err)
	}
	return raw
}

func mockReset(t *testing.T) {
	t.Helper()
	mockDo(t, "POST", "/_control/reset", "")
}

func mockScript(t *testing.T, to string, statuses ...int) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"to": to, "statuses": statuses})
	if err != nil {
		t.Fatal(err)
	}
	mockDo(t, "POST", "/_control/script", string(b))
}

type arrival struct {
	Key          string `json:"idempotency_key"`
	To           string `json:"to"`
	Channel      string `json:"channel"`
	Status       int    `json:"status"`
	Deduplicated bool   `json:"deduplicated"`
	At           string `json:"at"`
}

func mockArrivals(t *testing.T, query string) (arrs []arrival, delivered int) {
	t.Helper()
	raw := mockDo(t, "GET", "/_control/arrivals"+query, "")
	var out struct {
		Arrivals  []arrival `json:"arrivals"`
		Delivered int       `json:"delivered"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Arrivals, out.Delivered
}

func createNotif(t *testing.T, channel, recipient string) string {
	t.Helper()
	body := fmt.Sprintf(`{"recipient":%q,"channel":%q,"content":"engine e2e"}`, recipient, channel)
	code, _, n := call(t, "POST", "/v1/notifications", body, nil)
	if code != 201 {
		t.Fatalf("create: %d %v", code, n)
	}
	return n["id"].(string)
}

// composeCmd runs `docker compose <args>` from the repo root (no shell).
func composeCmd(args ...string) error { return composeEnv(nil, args...) }

// composeEnv is composeCmd with extra environment variables (they override the inherited ones).
func composeEnv(env []string, args ...string) error {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// stopWorker stops the worker and guarantees it is started again when the test ends.
func stopWorker(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := composeCmd("start", "worker"); err != nil {
			t.Errorf("restart worker: %v", err)
		}
	})
	if err := composeCmd("stop", "worker"); err != nil {
		t.Fatalf("stop worker: %v", err)
	}
}

func startWorker(t *testing.T) {
	t.Helper()
	if err := composeCmd("start", "worker"); err != nil {
		t.Fatalf("start worker: %v", err)
	}
}

func TestCreateToSent(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	to := emailRecipient()
	id := createNotif(t, "email", to)
	n := waitStatus(t, id, defaultDeadline, "sent")
	if n.Attempts != 1 || len(n.Delivery) != 1 || n.Delivery[0].HTTPStatus == nil || *n.Delivery[0].HTTPStatus != 202 {
		t.Fatalf("got %+v", n)
	}
	arrs, _ := mockArrivals(t, "?to="+to)
	if len(arrs) != 1 || arrs[0].Key != id {
		t.Fatalf("arrivals: %+v", arrs)
	}
}

func TestFiveXXThenSuccess(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	to := emailRecipient()
	mockScript(t, to, 503, 503, 202)
	n := waitStatus(t, createNotif(t, "email", to), defaultDeadline, "sent")
	if n.Attempts != 3 || len(n.Delivery) != 3 {
		t.Fatalf("got %+v", n)
	}
}

func Test4xxGoesFailed(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	to := emailRecipient()
	mockScript(t, to, 400)
	n := waitStatus(t, createNotif(t, "email", to), defaultDeadline, "failed")
	if n.Attempts != 1 || n.LastError == nil || !strings.HasPrefix(*n.LastError, "permanent: provider_4xx") {
		t.Fatalf("got %+v last_error=%v", n, n.LastError)
	}
}

func TestExhaustedGoesFailed(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	to := emailRecipient()
	mockScript(t, to, 503, 503, 503, 503, 503, 503, 503, 503)
	n := waitStatus(t, createNotif(t, "email", to), defaultDeadline, "failed")
	if n.Attempts != 8 || n.LastError == nil || !strings.HasPrefix(*n.LastError, "exhausted:") {
		t.Fatalf("got %+v last_error=%v", n, n.LastError)
	}
}

func Test429DoesNotConsumeAttempts(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	mockDo(t, "PUT", "/_control/config", `{"retry_after_s":1}`)
	to := emailRecipient()
	mockScript(t, to, 429, 429, 202)
	n := waitStatus(t, createNotif(t, "email", to), defaultDeadline, "sent")
	if n.Attempts != 1 || len(n.Delivery) != 3 {
		t.Fatalf("got %+v", n)
	}
}

func TestScheduledSendWaits(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	to := smsRecipient()
	at := time.Now().Add(6 * time.Second).UTC().Truncate(time.Millisecond)
	body := fmt.Sprintf(`{"recipient":%q,"channel":"sms","content":"scheduled e2e","scheduled_at":%q}`, to, at.Format(time.RFC3339Nano))
	code, _, created := call(t, "POST", "/v1/notifications", body, nil)
	if code != 201 {
		t.Fatalf("create: %d %v", code, created)
	}
	got, err := time.Parse(time.RFC3339Nano, fmt.Sprint(created["scheduled_at"]))
	if err != nil || !got.Equal(at) {
		t.Fatalf("scheduled_at = %v, want %v (err %v)", created["scheduled_at"], at, err)
	}
	id := created["id"].(string)

	time.Sleep(3 * time.Second)
	if n := getNotif(t, id); n.Status != "pending" || n.Attempts != 0 {
		t.Fatalf("before its time: %+v", n)
	}
	if arrs, _ := mockArrivals(t, "?to="+url.QueryEscape(to)); len(arrs) != 0 {
		t.Fatalf("provider received it before scheduled_at: %+v", arrs)
	}
	waitStatus(t, id, 20*time.Second, "sent")
	arrs, _ := mockArrivals(t, "?to="+url.QueryEscape(to))
	if len(arrs) != 1 || arrs[0].Key != id {
		t.Fatalf("arrivals: %+v", arrs)
	}
}

// startSingleSenderWorker recreates the worker with one sender per channel (PF15): it drains
// each claimed batch in claim order, so provider arrival order equals claim order. The cleanup
// recreates it with the default concurrency again.
func startSingleSenderWorker(t *testing.T) {
	t.Helper()
	up := []string{"up", "-d", "--no-deps", "worker"}
	t.Cleanup(func() {
		if err := composeEnv([]string{"WORKER_CONCURRENCY_PER_CHANNEL=10"}, up...); err != nil {
			t.Errorf("restore worker concurrency: %v", err)
		}
	})
	if err := composeEnv([]string{"WORKER_CONCURRENCY_PER_CHANNEL=1"}, up...); err != nil {
		t.Fatalf("start single sender worker: %v", err)
	}
}

func TestPriorityOrder(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	stopWorker(t)
	prefix := "prio-" + uniq() + "-"
	var items []string
	for _, p := range []string{"low", "high"} {
		for i := 0; i < 30; i++ {
			items = append(items, fmt.Sprintf(`{"recipient":"%s%s-%d","channel":"push","content":"p","priority":%q}`, prefix, p, i, p))
		}
	}
	code, _, created := call(t, "POST", "/v1/notifications/batch", `{"notifications":[`+strings.Join(items, ",")+`]}`, nil)
	if code != 201 {
		t.Fatalf("batch: %d %v", code, created)
	}
	ids := created["notification_ids"].([]any)
	startSingleSenderWorker(t)
	for _, id := range ids {
		waitStatus(t, id.(string), 60*time.Second, "sent")
	}
	arrs, _ := mockArrivals(t, "?channel=push")
	var mine []arrival
	for _, a := range arrs {
		if strings.HasPrefix(a.To, prefix) {
			mine = append(mine, a)
		}
	}
	if len(mine) < 60 {
		t.Fatalf("got %d arrivals, want at least 60", len(mine))
	}
	// One sender per channel: arrival order is claim order, so all 30 high rows precede any low row.
	for i, a := range mine[:30] {
		if !strings.HasPrefix(a.To, prefix+"high-") {
			t.Fatalf("arrival %d went to %s, want a high priority row first", i, strings.TrimPrefix(a.To, prefix))
		}
	}
}

func TestCancelPendingBeforeClaim(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	stopWorker(t)
	to := emailRecipient()
	id := createNotif(t, "email", to)
	if code, _, c := call(t, "POST", "/v1/notifications/"+id+"/cancel", "", nil); code != 200 || c["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", code, c)
	}
	canary := createNotif(t, "email", emailRecipient())
	startWorker(t)
	waitStatus(t, canary, 90*time.Second, "sent")
	if n := getNotif(t, id); n.Status != "cancelled" || n.Attempts != 0 {
		t.Fatalf("got %+v", n)
	}
	if arrs, _ := mockArrivals(t, "?to="+to); len(arrs) != 0 {
		t.Fatalf("cancelled notification reached the provider: %+v", arrs)
	}
}

func TestRateSmoke(t *testing.T) {
	needEnginePhase(t)
	mockReset(t)
	const batchSize, singles = 1000, 200
	items := make([]string, batchSize)
	for i := range items {
		items[i] = fmt.Sprintf(`{"recipient":%q,"channel":"sms","content":"rate"}`, smsRecipient())
	}
	code, _, created := call(t, "POST", "/v1/notifications/batch", `{"notifications":[`+strings.Join(items, ",")+`]}`, nil)
	if code != 201 {
		t.Fatalf("batch: %d", code)
	}
	ids := created["notification_ids"].([]any)
	for i := 0; i < singles; i++ {
		ids = append(ids, createNotif(t, "sms", smsRecipient()))
	}
	total := len(ids)

	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, delivered := mockArrivals(t, "?channel=sms"); delivered >= total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not all %d sms delivered within 60s", total)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, id := range ids {
		waitStatus(t, id.(string), 30*time.Second, "sent")
	}

	arrs, _ := mockArrivals(t, "?channel=sms")
	times := make([]time.Time, 0, len(arrs))
	for _, a := range arrs {
		ts, err := time.Parse(time.RFC3339Nano, a.At)
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, ts)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	if len(times) < total {
		t.Fatalf("%d arrivals for %d notifications", len(times), total)
	}
	span := times[len(times)-1].Sub(times[0]).Seconds()
	if avg := float64(len(times)) / span; avg > 105 {
		t.Fatalf("average %.1f/s over %.1fs exceeds 100/s plus 5 percent", avg, span)
	}
	maxWin, lo := 0, 0
	for hi := range times {
		for times[hi].Sub(times[lo]) >= time.Second {
			lo++
		}
		if w := hi - lo + 1; w > maxWin {
			maxWin = w
		}
	}
	if maxWin > 105 {
		t.Fatalf("a 1s window held %d arrivals, want at most 105", maxWin)
	}
	t.Logf("%d arrivals in %.1fs (avg %.1f/s), max 1s window %d", len(times), span, float64(len(times))/span, maxWin)
}
