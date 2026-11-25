// Package provider talks to the external delivery provider and turns every
// outcome into a Result the retry policy can act on.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Class is the delivery outcome category of one provider call.
type Class int

const (
	Success Class = iota
	Throttled
	Retryable
	Permanent
)

// String returns the lowercase name, used as the `result` metric label.
func (c Class) String() string {
	switch c {
	case Success:
		return "success"
	case Throttled:
		return "throttled"
	case Retryable:
		return "retryable"
	case Permanent:
		return "permanent"
	}
	return "unknown"
}

// Fixed error codes. Never built from error text.
const (
	CodeThrottled        = "provider_throttled"
	Code5xx              = "provider_5xx"
	CodeTimeout          = "provider_timeout"
	CodeCanceled         = "provider_canceled"
	CodeConnError        = "provider_conn_error"
	Code408              = "provider_408"
	Code425              = "provider_425"
	Code4xx              = "provider_4xx"
	Code3xx              = "provider_3xx"
	CodeUnexpectedStatus = "provider_unexpected_status"
)

const (
	minRetryAfter = time.Second
	maxRetryAfter = 60 * time.Second
	maxLastError  = 512
)

// Result is the classified outcome of one provider call.
type Result struct {
	Class      Class
	MessageID  string
	HTTPStatus int // 0 when no response arrived
	RetryAfter time.Duration
	ErrCode    string // empty on success
	Detail     string // short fixed text, never derived from a net/http error string
}

// LastError is the value stored in notifications.last_error: empty on
// success, otherwise "<code>: <detail>" truncated to 512 runes.
func (r Result) LastError() string {
	if r.Class == Success {
		return ""
	}
	s := r.ErrCode + ": " + r.Detail
	rs := []rune(s)
	if len(rs) > maxLastError {
		return string(rs[:maxLastError])
	}
	return s
}

// Classify maps an HTTP response to a Result. It is pure; the unparsed-body
// side effects (counter, log) live in the Client.
func Classify(status int, header http.Header, body []byte) Result {
	r, _ := classifyAt(time.Now(), status, header, body)
	return r
}

// classifyAt is Classify with an injectable clock. The bool reports a 2xx
// whose body did not yield a messageId.
func classifyAt(now time.Time, status int, header http.Header, body []byte) (Result, bool) {
	res := Result{HTTPStatus: status}
	switch {
	case status >= 200 && status <= 299:
		res.Class = Success
		var parsed struct {
			MessageID string `json:"messageId"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil || !storableMessageID(parsed.MessageID) {
			return res, true
		}
		res.MessageID = parsed.MessageID
		return res, false
	case status == http.StatusTooManyRequests:
		res.Class, res.ErrCode = Throttled, CodeThrottled
		res.RetryAfter = parseRetryAfter(now, header.Get("Retry-After"))
	case status == http.StatusRequestTimeout:
		res.Class, res.ErrCode = Retryable, Code408
	case status == http.StatusTooEarly:
		res.Class, res.ErrCode = Retryable, Code425
	case status >= 500 && status <= 599:
		res.Class, res.ErrCode = Retryable, Code5xx
	case status >= 400 && status <= 499:
		res.Class, res.ErrCode = Permanent, Code4xx
	case status >= 300 && status <= 399:
		res.Class, res.ErrCode = Permanent, Code3xx
	default:
		res.Class, res.ErrCode = Permanent, CodeUnexpectedStatus
	}
	res.Detail = "provider responded with status " + strconv.Itoa(status)
	return res, false
}

// maxMessageIDBytes bounds the provider id kept on the notification row.
const maxMessageIDBytes = 256

// storableMessageID reports whether id is non-empty, at most maxMessageIDBytes
// and free of control characters. Postgres text rejects NUL, so an id carrying
// one would fail the sent write and the reaper would send the message again.
func storableMessageID(id string) bool {
	if id == "" || len(id) > maxMessageIDBytes {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// parseRetryAfter reads delta seconds or an HTTP date and clamps to 1s..60s.
// A missing or invalid value yields 0.
func parseRetryAfter(now time.Time, v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0
		}
		if secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	} else {
		return 0
	}
	if d < minRetryAfter {
		return minRetryAfter
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// ClassifyError maps a transport error to a Result. The error text is never
// copied because a *url.Error carries the request URL.
func ClassifyError(err error) Result {
	res := Result{Class: Retryable}
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		res.ErrCode, res.Detail = CodeCanceled, "request canceled before a response arrived"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		res.ErrCode, res.Detail = CodeTimeout, "request timed out"
	default:
		res.ErrCode, res.Detail = CodeConnError, "transport error before a response arrived"
	}
	return res
}
