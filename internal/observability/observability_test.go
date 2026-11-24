package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"

	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

func TestLoggerAddsCorrelationID(t *testing.T) {
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "info")
	ctx := observability.WithCorrelationID(context.Background(), "req-1")
	log.InfoContext(ctx, "hello", "notification_id", "n1")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if m["correlation_id"] != "req-1" || m["msg"] != "hello" || m["notification_id"] != "n1" {
		t.Fatalf("unexpected record: %v", m)
	}
}

func TestLoggerKeepsCorrelationIDThroughWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "info").With("component", "worker").WithGroup("g")
	ctx := observability.WithCorrelationID(context.Background(), "req-2")
	log.InfoContext(ctx, "hi", "k", "v")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if m["component"] != "worker" {
		t.Fatalf("With attrs lost: %v", m)
	}
	// After WithGroup the correlation id lands inside the group; it must still be present.
	g, ok := m["g"].(map[string]any)
	if !ok || g["correlation_id"] != "req-2" || g["k"] != "v" {
		t.Fatalf("group record wrong: %v", m)
	}
}

func TestLoggerWithoutCorrelationIDOmitsField(t *testing.T) {
	var buf bytes.Buffer
	observability.NewLogger(&buf, "info").Info("x")
	if strings.Contains(buf.String(), "correlation_id") {
		t.Error("field must be absent without an id")
	}
}

func TestLoggerLevel(t *testing.T) {
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "warn")
	log.Info("quiet")
	if buf.Len() != 0 {
		t.Error("info must be filtered at warn level")
	}
	log.Warn("loud")
	if buf.Len() == 0 {
		t.Error("warn must pass")
	}
}

func TestLoggerUnknownLevelFallsBackToInfo(t *testing.T) {
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "bogus")
	log.Debug("hidden")
	if buf.Len() != 0 {
		t.Error("debug must be filtered at fallback info level")
	}
	log.Info("shown")
	if buf.Len() == 0 {
		t.Error("info must pass at fallback level")
	}
}

func TestValidRequestID(t *testing.T) {
	good := []string{"abc-123._", "a", strings.Repeat("a", 64)}
	bad := []string{"", strings.Repeat("a", 65), "a b", "a\nb", "é", "a/b"}
	for _, s := range good {
		if !observability.ValidRequestID(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range bad {
		if observability.ValidRequestID(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestNewRequestIDIsValidAndUnique(t *testing.T) {
	a, b := observability.NewRequestID(), observability.NewRequestID()
	if !observability.ValidRequestID(a) || a == b {
		t.Fatalf("bad ids %q %q", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("want 32 hex chars, got %d", len(a))
	}
}

func TestCorrelationIDEmptyByDefault(t *testing.T) {
	if got := observability.CorrelationID(context.Background()); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func attrMap(attrs []any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(attrs); i += 2 {
		m[attrs[i].(string)] = attrs[i+1]
	}
	return m
}

func TestErrAttrsNeverCarriesErrorText(t *testing.T) {
	const secret = "SENSITIVE-recipient-+905551234567"
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "info")
	plain := fmt.Errorf("wrap: %w", errors.New(secret))
	log.Error("failed", observability.ErrAttrs(plain)...)
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("raw error text leaked: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "err_type") || !strings.Contains(buf.String(), "*fmt.wrapError") {
		t.Fatalf("err_type missing: %s", buf.String())
	}
}

func TestErrAttrsPgError(t *testing.T) {
	const secret = "Failing row contains (+905551234567, hello)"
	pg := &pgconn.PgError{Code: "23514", ConstraintName: "notifications_content_check", Detail: secret, Message: secret}
	err := fmt.Errorf("insert: %w", pg)
	m := attrMap(observability.ErrAttrs(err))
	if m["err_type"] != "*fmt.wrapError" || m["code"] != "23514" || m["constraint"] != "notifications_content_check" {
		t.Fatalf("attrs wrong: %v", m)
	}
	var buf bytes.Buffer
	observability.NewLogger(&buf, "info").Error("db", observability.ErrAttrs(err)...)
	if strings.Contains(buf.String(), "+9055") || strings.Contains(buf.String(), "Failing row") {
		t.Fatalf("pg detail leaked: %s", buf.String())
	}
}

type customErr struct{ msg string }

func (e *customErr) Error() string { return e.msg }

func TestErrAttrsClassifiesCauses(t *testing.T) {
	const secret = "SENSITIVE-row-data"
	wrap := func(err error) error { return fmt.Errorf("claim %s: %w", secret, err) }
	cases := []struct {
		name      string
		err       error
		class     string
		wantExtra map[string]any
	}{
		{"canceled", wrap(context.Canceled), "context_canceled", nil},
		{"deadline", wrap(context.DeadlineExceeded), "deadline_exceeded", nil},
		{"net op", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New(secret)}), "net_op_error", map[string]any{"net_op": "dial"}},
		{"connect", wrap(&pgconn.ConnectError{}), "pg_connect_error", nil},
		{"pool exhausted", wrap(puddle.ErrNotAvailable), "pool_exhausted", nil},
		{"pool closed", wrap(puddle.ErrClosedPool), "pool_closed", nil},
		{"sqlstate", wrap(&pgconn.PgError{Code: "40001", Message: secret}), "pg_error", map[string]any{"code": "40001"}},
		{"innermost type", wrap(fmt.Errorf("mid: %w", &customErr{secret})), "*observability_test.customErr", nil},
	}
	for _, c := range cases {
		attrs := observability.ErrAttrs(c.err)
		m := attrMap(attrs)
		if m["err_class"] != c.class {
			t.Errorf("%s: err_class = %v, want %s", c.name, m["err_class"], c.class)
		}
		for k, v := range c.wantExtra {
			if m[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.name, k, m[k], v)
			}
		}
		var buf bytes.Buffer
		observability.NewLogger(&buf, "info").Error("x", attrs...)
		if strings.Contains(buf.String(), secret) {
			t.Errorf("%s: error text leaked: %s", c.name, buf.String())
		}
	}
}

func TestErrAttrsNil(t *testing.T) {
	if got := observability.ErrAttrs(nil); len(got) != 0 {
		t.Fatalf("nil error must yield no attrs, got %v", got)
	}
}

func TestPanicAttrsRecordsTypeOnly(t *testing.T) {
	const secret = "SENSITIVE-content"
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "info")
	log.Error("panic", observability.PanicAttrs(secret)...)
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("panic value leaked: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"panic_type":"string"`) {
		t.Fatalf("panic_type missing: %s", buf.String())
	}
}
