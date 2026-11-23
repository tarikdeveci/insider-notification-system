package api_test

import (
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/testutil"
)

func TestRetryEndpoint(t *testing.T) {
	pool := testutil.MigratedPool(t)
	h := api.NewServer(api.Deps{Store: store.New(pool), DB: pool, Redis: fakePinger{}, Logger: discardLogger()})
	id := obj(t, call(t, h, "POST", "/v1/notifications", smsBody, nil))["id"].(string)

	// pending: 409
	rr := call(t, h, "POST", "/v1/notifications/"+id+"/retry", "", nil)
	if rr.Code != 409 || obj(t, rr)["detail"] == nil && obj(t, rr)["title"] == nil {
		t.Fatalf("pending retry: %d %s", rr.Code, rr.Body)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE notifications SET status='failed', attempts=8, last_error='x' WHERE id=$1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	rr = call(t, h, "POST", "/v1/notifications/"+id+"/retry", "", nil)
	if rr.Code != 200 {
		t.Fatalf("retry: %d %s", rr.Code, rr.Body)
	}
	m := obj(t, rr)
	if m["status"] != "pending" || m["attempts"].(float64) != 0 || m["id"] != id {
		t.Fatalf("body: %v", m)
	}
	if rr := call(t, h, "POST", "/v1/notifications/"+id+"/retry", "", nil); rr.Code != 409 {
		t.Fatalf("second retry: %d", rr.Code)
	}
}

func TestRetryMissingOrMalformedIs404(t *testing.T) {
	h := newAPI(t)
	for _, p := range []string{
		"/v1/notifications/123e4567-e89b-12d3-a456-426614174000/retry",
		"/v1/notifications/not-a-uuid/retry",
	} {
		if rr := call(t, h, "POST", p, "", nil); rr.Code != 404 {
			t.Errorf("POST %s: %d", p, rr.Code)
		}
	}
}
