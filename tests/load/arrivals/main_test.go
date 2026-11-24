package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, body string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestChannelWithoutArrivalsFails(t *testing.T) {
	url := serve(t, `{"arrivals":[{"channel":"sms","at":"2026-01-01T00:00:00Z"},{"channel":"email","at":"2026-01-01T00:00:00Z"}]}`)
	err := run(url, 105, 100)
	if err == nil {
		t.Fatalf("want failure for channel without arrivals, got %v", err)
	}
}

func TestAllChannelsWithinLimitPasses(t *testing.T) {
	url := serve(t, `{"arrivals":[{"channel":"sms","at":"2026-01-01T00:00:00Z"},{"channel":"email","at":"2026-01-01T00:00:00Z"},{"channel":"push","at":"2026-01-01T00:00:00Z"}]}`)
	if err := run(url, 105, 100); err != nil {
		t.Fatal(err)
	}
}
