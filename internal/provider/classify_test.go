package provider

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var fixedNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func TestClassify2xxIsSuccess(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204, 299} {
		r, _ := classifyAt(fixedNow, status, nil, []byte(`{"messageId":"abc-1"}`))
		if r.Class != Success || r.ErrCode != "" || r.HTTPStatus != status {
			t.Fatalf("status %d: got %+v", status, r)
		}
		if r.MessageID != "abc-1" {
			t.Fatalf("status %d: message id %q", status, r.MessageID)
		}
		if r.LastError() != "" {
			t.Fatalf("success LastError must be empty, got %q", r.LastError())
		}
	}
	for _, body := range []string{"", "not json", `{"messageId":""}`, `{"other":1}`, `[1]`} {
		r, unparsed := classifyAt(fixedNow, 202, nil, []byte(body))
		if r.Class != Success || r.MessageID != "" || !unparsed {
			t.Fatalf("body %q: got %+v unparsed=%v", body, r, unparsed)
		}
	}
	if _, unparsed := classifyAt(fixedNow, 202, nil, []byte(`{"messageId":"x"}`)); unparsed {
		t.Fatal("parsed body flagged unparsed")
	}
}

func TestClassify429(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"missing", "", 0},
		{"invalid", "soon", 0},
		{"negative", "-5", 0},
		{"seconds", "7", 7 * time.Second},
		{"zero clamps up", "0", time.Second},
		{"large clamps down", "3600", 60 * time.Second},
		{"http date", fixedNow.Add(30 * time.Second).UTC().Format(http.TimeFormat), 30 * time.Second},
		{"past date clamps up", fixedNow.Add(-time.Hour).UTC().Format(http.TimeFormat), time.Second},
		{"far date clamps down", fixedNow.Add(time.Hour).UTC().Format(http.TimeFormat), 60 * time.Second},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		r, _ := classifyAt(fixedNow, 429, h, nil)
		if r.Class != Throttled || r.ErrCode != "provider_throttled" || r.RetryAfter != c.want {
			t.Errorf("%s: got %+v want retryAfter %v", c.name, r, c.want)
		}
	}
}

func TestClassify5xxTimeoutConnError(t *testing.T) {
	for _, status := range []int{500, 502, 503, 599} {
		r, _ := classifyAt(fixedNow, status, nil, nil)
		if r.Class != Retryable || r.ErrCode != "provider_5xx" {
			t.Fatalf("%d: %+v", status, r)
		}
	}
}

func TestClassify408And425AreRetryable(t *testing.T) {
	for status, code := range map[int]string{408: "provider_408", 425: "provider_425"} {
		r, _ := classifyAt(fixedNow, status, nil, nil)
		if r.Class != Retryable || r.ErrCode != code || r.HTTPStatus != status {
			t.Fatalf("%d: %+v", status, r)
		}
	}
	if r, _ := classifyAt(fixedNow, 429, nil, nil); r.Class != Throttled || r.ErrCode != CodeThrottled {
		t.Fatalf("429 must stay throttled: %+v", r)
	}
}

func TestClassifyOther4xxAnd3xx(t *testing.T) {
	for _, status := range []int{400, 401, 404, 422} {
		r, _ := classifyAt(fixedNow, status, nil, nil)
		if r.Class != Permanent || r.ErrCode != "provider_4xx" {
			t.Fatalf("%d: %+v", status, r)
		}
	}
	for _, status := range []int{300, 301, 302, 307, 308} {
		r, _ := classifyAt(fixedNow, status, nil, nil)
		if r.Class != Permanent || r.ErrCode != "provider_3xx" {
			t.Fatalf("%d: %+v", status, r)
		}
	}
	for _, status := range []int{100, 199, 600, 0} {
		r, _ := classifyAt(fixedNow, status, nil, nil)
		if r.Class != Permanent || r.ErrCode != "provider_unexpected_status" {
			t.Fatalf("%d: %+v", status, r)
		}
	}
}

func TestClassString(t *testing.T) {
	want := map[Class]string{Success: "success", Throttled: "throttled", Retryable: "retryable", Permanent: "permanent"}
	for c, s := range want {
		if c.String() != s {
			t.Fatalf("%d: %q", c, c.String())
		}
	}
}

func TestLastErrorTruncatesRuneSafe(t *testing.T) {
	r := Result{Class: Permanent, ErrCode: "provider_4xx", Detail: strings.Repeat("ğ", 600)}
	got := r.LastError()
	if utf8.RuneCountInString(got) != 512 || !utf8.ValidString(got) {
		t.Fatalf("runes=%d valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	short := Result{Class: Retryable, ErrCode: "provider_5xx", Detail: "x"}
	if short.LastError() != "provider_5xx: x" {
		t.Fatalf("got %q", short.LastError())
	}
}
