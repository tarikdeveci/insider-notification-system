package provider

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const marker = "SECRET-UUID-MARKER"

func newTestClient(t *testing.T, url string, opts ...Option) *Client {
	t.Helper()
	c, err := NewClient(url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientSendsIdempotencyKey(t *testing.T) {
	var gotKey, gotCT, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		gotCT = r.Header.Get("Content-Type")
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"messageId":"m-1","status":"accepted"}`))
	}))
	defer srv.Close()

	res := newTestClient(t, srv.URL+"/"+marker).Send(context.Background(),
		Message{ID: "n-123", To: "+905551234567", Channel: "sms", Content: "hello"})
	if res.Class != Success || res.MessageID != "m-1" || res.HTTPStatus != 202 {
		t.Fatalf("got %+v", res)
	}
	if gotKey != "n-123" || gotCT != "application/json" || gotMethod != http.MethodPost {
		t.Fatalf("key=%q ct=%q method=%q", gotKey, gotCT, gotMethod)
	}
	if len(gotBody) != 3 || gotBody["to"] != "+905551234567" || gotBody["channel"] != "sms" || gotBody["content"] != "hello" {
		t.Fatalf("body %v", gotBody)
	}
}

func TestClientUnparsedBodyIncrementsCounter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "provider_unparsed_body_total"})
	var logs strings.Builder
	lg := slog.New(slog.NewTextHandler(&logs, nil))
	c := newTestClient(t, srv.URL, WithUnparsedCounter(counter), WithLogger(lg))
	res := c.Send(context.Background(), Message{ID: "n-9", To: "rcpt-secret", Channel: "email", Content: "body-secret"})
	if res.Class != Success || res.MessageID != "" {
		t.Fatalf("got %+v", res)
	}
	if v := testutil.ToFloat64(counter); v != 1 {
		t.Fatalf("counter %v", v)
	}
	out := logs.String()
	if !strings.Contains(out, "notification_id=n-9") || !strings.Contains(out, "http_status=200") {
		t.Fatalf("warning missing fields: %q", out)
	}
	for _, bad := range []string{"rcpt-secret", "body-secret", "<html>", srv.URL} {
		if strings.Contains(out, bad) {
			t.Fatalf("log leaks %q", bad)
		}
	}
}

func TestClientTimeoutIs5s(t *testing.T) {
	c := newTestClient(t, "http://example.invalid/x")
	if c.Timeout() != 5*time.Second {
		t.Fatalf("default timeout %v", c.Timeout())
	}
	if c.http.Timeout != 5*time.Second {
		t.Fatalf("http.Client timeout %v", c.http.Timeout)
	}
}

func TestClientTimeoutIsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	res := newTestClient(t, srv.URL, WithTimeout(50*time.Millisecond)).Send(context.Background(), Message{ID: "1"})
	if res.Class != Retryable || res.ErrCode != "provider_timeout" || res.HTTPStatus != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestClientContextDeadlineIsTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res := newTestClient(t, srv.URL).Send(ctx, Message{ID: "1"})
	if res.Class != Retryable || res.ErrCode != "provider_timeout" {
		t.Fatalf("got %+v", res)
	}
}

func TestClientCanceled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	res := newTestClient(t, srv.URL).Send(ctx, Message{ID: "1"})
	if res.Class != Retryable || res.ErrCode != "provider_canceled" {
		t.Fatalf("got %+v", res)
	}
}

func TestClientConnRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	res := newTestClient(t, "http://"+addr+"/"+marker).Send(context.Background(), Message{ID: "1"})
	if res.Class != Retryable || res.ErrCode != "provider_conn_error" || res.HTTPStatus != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	res := newTestClient(t, srv.URL).Send(context.Background(), Message{ID: "1"})
	if res.Class != Permanent || res.ErrCode != "provider_3xx" || res.HTTPStatus != 302 {
		t.Fatalf("got %+v", res)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestClientBodyLimit64KiB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		// Valid JSON whose closing quote lies beyond 64 KiB, so a limited read cannot parse it.
		_, _ = io.WriteString(w, `{"messageId":"`+strings.Repeat("a", 70<<10)+`"}`)
	}))
	defer srv.Close()
	res := newTestClient(t, srv.URL).Send(context.Background(), Message{ID: "1"})
	if res.Class != Success || res.MessageID != "" {
		t.Fatalf("oversize body must be truncated and unparsed, got class=%v idlen=%d", res.Class, len(res.MessageID))
	}
}

func TestNewClientValidatesURL(t *testing.T) {
	bad := []string{
		"", "ftp://host/x", "file:///etc/passwd", "http://", "http:///path",
		"http://uzer-x:pw-x@host.example/" + marker, "://nope", "host.example/path",
		"http://uzer-x@host.example/x",
	}
	for _, u := range bad {
		_, err := NewClient(u)
		if err == nil {
			t.Errorf("%q accepted", u)
			continue
		}
		for _, leak := range []string{"uzer-x", "pw-x", marker, "host.example", "ftp", "nope"} {
			if strings.Contains(u, leak) && strings.Contains(err.Error(), leak) {
				t.Errorf("error for %q leaks %q: %v", u, leak, err)
			}
		}
	}
	for _, u := range []string{"http://localhost:8080/x", "https://example.com/a/b?q=1"} {
		if _, err := NewClient(u); err != nil {
			t.Errorf("%q rejected: %v", u, err)
		}
	}
}

func TestClientErrorsNeverContainURL(t *testing.T) {
	var status atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status.Load() == 0 {
			<-release
			return
		}
		w.Header().Set("Location", "/"+marker)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	defer close(release)
	url := srv.URL + "/" + marker
	host := strings.TrimPrefix(srv.URL, "http://")

	check := func(name string, res Result) {
		t.Helper()
		got := res.LastError()
		if got == "" {
			t.Errorf("%s: empty LastError", name)
		}
		for _, leak := range []string{marker, host, "http://", "127.0.0.1"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: leaks %q: %q", name, leak, got)
			}
		}
	}

	check("timeout", newTestClient(t, url, WithTimeout(40*time.Millisecond)).Send(context.Background(), Message{ID: "1"}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	check("canceled", newTestClient(t, url).Send(ctx, Message{ID: "1"}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	check("refused", newTestClient(t, "http://"+addr+"/"+marker).Send(context.Background(), Message{ID: "1"}))

	for _, s := range []int{500, 400, 302} {
		status.Store(int32(s))
		check("status", newTestClient(t, url).Send(context.Background(), Message{ID: "1"}))
	}
}
