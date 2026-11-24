package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

// fakeClock is an injectable clock the test can move forward.
type fakeClock struct{ ns atomic.Int64 }

func newClock(t time.Time) *fakeClock { c := &fakeClock{}; c.ns.Store(t.UnixNano()); return c }
func (c *fakeClock) Now() time.Time   { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Set(t time.Time)  { c.ns.Store(t.UnixNano()) }

func newAPIWithClock(t *testing.T, c *fakeClock) http.Handler {
	t.Helper()
	pool := testutil.MigratedPool(t)
	return api.NewServer(api.Deps{Store: store.New(pool), DB: pool, Logger: discardLogger(), Now: c.Now})
}

// sameInstant reports whether v is an RFC 3339 string for the instant want
// (responses render in the server's local zone, so strings are not compared).
func sameInstant(v any, want string) bool {
	got, ok := v.(string)
	if !ok {
		return false
	}
	g, err1 := time.Parse(time.RFC3339Nano, got)
	w, err2 := time.Parse(time.RFC3339, want)
	return err1 == nil && err2 == nil && g.Equal(w)
}

func schedSMS(at string) string {
	return fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","content":"hello","scheduled_at":%q}`, at)
}

func TestScheduledAtShownOnEveryResponse(t *testing.T) {
	clock := newClock(time.Now())
	h := newAPIWithClock(t, clock)
	at := clock.Now().Add(time.Hour).Truncate(time.Second)
	rr := call(t, h, "POST", "/v1/notifications", schedSMS(at.Format(time.RFC3339)), nil)
	if rr.Code != 201 {
		t.Fatalf("code %d: %s", rr.Code, rr.Body)
	}
	m := obj(t, rr)
	if !sameInstant(m["scheduled_at"], at.Format(time.RFC3339)) || !sameInstant(m["next_attempt_at"], at.Format(time.RFC3339)) {
		t.Fatalf("create body: %v", m)
	}
	id := m["id"].(string)
	if got := obj(t, call(t, h, "GET", "/v1/notifications/"+id, "", nil)); got["scheduled_at"] != m["scheduled_at"] {
		t.Fatalf("get: %v", got)
	}
	list := obj(t, call(t, h, "GET", "/v1/notifications", "", nil))["items"].([]any)
	if list[0].(map[string]any)["scheduled_at"] != m["scheduled_at"] {
		t.Fatalf("list: %v", list[0])
	}
	if c := obj(t, call(t, h, "POST", "/v1/notifications/"+id+"/cancel", "", nil)); c["scheduled_at"] != m["scheduled_at"] {
		t.Fatalf("cancel: %v", c)
	}
	plain := obj(t, call(t, h, "POST", "/v1/notifications", smsBody, nil))
	if v, ok := plain["scheduled_at"]; !ok || v != nil {
		t.Fatalf("unscheduled must render scheduled_at null: %v", plain)
	}
}

func TestScheduledAtRejections(t *testing.T) {
	clock := newClock(time.Now())
	h := newAPIWithClock(t, clock)
	now := clock.Now()
	for name, at := range map[string]string{
		"past":      now.Add(-time.Minute).Format(time.RFC3339),
		"too far":   now.Add(31 * 24 * time.Hour).Format(time.RFC3339),
		"bad":       "tomorrow",
		"date only": "2030-01-01",
	} {
		rr := call(t, h, "POST", "/v1/notifications", schedSMS(at), nil)
		if rr.Code != 422 {
			t.Errorf("%s: code %d", name, rr.Code)
			continue
		}
		e := obj(t, rr)["errors"].([]any)[0].(map[string]any)
		if e["field"] != "scheduled_at" {
			t.Errorf("%s: %v", name, e)
		}
	}
	if rr := call(t, h, "POST", "/v1/notifications", `{"recipient":"+905551234567","channel":"sms","content":"x","scheduled_at":5}`, nil); rr.Code != 400 && rr.Code != 422 {
		t.Errorf("non string scheduled_at: %d", rr.Code)
	}
}

func TestReplayAfterScheduledTimeStillReturnsOriginal(t *testing.T) {
	clock := newClock(time.Now())
	h := newAPIWithClock(t, clock)
	at := clock.Now().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	key := map[string]string{"Idempotency-Key": "sched-1"}
	first := call(t, h, "POST", "/v1/notifications", schedSMS(at), key)
	if first.Code != 201 {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	id := obj(t, first)["id"]

	clock.Set(clock.Now().Add(2 * time.Hour)) // scheduled time is now in the past
	again := call(t, h, "POST", "/v1/notifications", schedSMS(at), key)
	if again.Code != 200 || obj(t, again)["id"] != id {
		t.Fatalf("replay after time: %d %s", again.Code, again.Body)
	}
	other := call(t, h, "POST", "/v1/notifications", schedSMS(clock.Now().Add(-time.Minute).Format(time.RFC3339)), key)
	if other.Code != 409 {
		t.Fatalf("different body after time: %d %s", other.Code, other.Body)
	}
	fresh := call(t, h, "POST", "/v1/notifications", schedSMS(at), map[string]string{"Idempotency-Key": "sched-2"})
	if fresh.Code != 422 {
		t.Fatalf("new key with past time: %d %s", fresh.Code, fresh.Body)
	}
	if nokey := call(t, h, "POST", "/v1/notifications", schedSMS(at), nil); nokey.Code != 422 {
		t.Fatalf("no key with past time: %d", nokey.Code)
	}
}

func TestBatchScheduled(t *testing.T) {
	clock := newClock(time.Now())
	h := newAPIWithClock(t, clock)
	at := clock.Now().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)
	item := func(extra string) string {
		return `{"recipient":"+905551234567","channel":"sms","content":"m"` + extra + `}`
	}
	body := `{"notifications":[` + item("") + `,` + item(`,"scheduled_at":"`+at+`"`) + `]}`
	key := map[string]string{"Idempotency-Key": "bsched"}
	rr := call(t, h, "POST", "/v1/notifications/batch", body, key)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	ids := obj(t, rr)["notification_ids"].([]any)
	if got := obj(t, call(t, h, "GET", "/v1/notifications/"+ids[0].(string), "", nil)); got["scheduled_at"] != nil {
		t.Fatalf("item 0: %v", got)
	}
	if got := obj(t, call(t, h, "GET", "/v1/notifications/"+ids[1].(string), "", nil)); !sameInstant(got["scheduled_at"], at) || !sameInstant(got["next_attempt_at"], at) {
		t.Fatalf("item 1: %v", got)
	}

	clock.Set(clock.Now().Add(2 * time.Hour))
	if again := call(t, h, "POST", "/v1/notifications/batch", body, key); again.Code != 200 || obj(t, again)["batch_id"] != obj(t, rr)["batch_id"] {
		t.Fatalf("replay after time: %d %s", again.Code, again.Body)
	}
	changed := `{"notifications":[` + item("") + `]}`
	if c := call(t, h, "POST", "/v1/notifications/batch", changed, key); c.Code != 409 {
		t.Fatalf("different body: %d", c.Code)
	}
	// A different body that also fails the schedule check must still be a 409
	// (batchScheduleRejected resolves the stored key before showing 422).
	pastChanged := `{"notifications":[` + item(`,"scheduled_at":"`+at+`"`) + `]}`
	if c := call(t, h, "POST", "/v1/notifications/batch", pastChanged, key); c.Code != 409 {
		t.Fatalf("different body with past schedule: %d %s", c.Code, c.Body)
	}
	fresh := call(t, h, "POST", "/v1/notifications/batch", body, map[string]string{"Idempotency-Key": "bsched2"})
	if fresh.Code != 422 {
		t.Fatalf("new key past time: %d %s", fresh.Code, fresh.Body)
	}
	e := obj(t, fresh)["errors"].([]any)[0].(map[string]any)
	if e["index"].(float64) != 1 || e["field"] != "scheduled_at" {
		t.Fatalf("error: %v", e)
	}
	if !strings.Contains(fresh.Header().Get("Content-Type"), "problem+json") {
		t.Fatalf("content type %s", fresh.Header().Get("Content-Type"))
	}
}
