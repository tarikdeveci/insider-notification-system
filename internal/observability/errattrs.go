package observability

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// ErrAttrs returns slog key/value pairs describing err without ever including
// err.Error(), which can carry row data (recipient, content) from Postgres or
// wrapped errors. It records:
//   - err_type: %T of err (often only *fmt.wrapError for wrapped errors);
//   - err_class: a data free category, see errClass;
//   - net_op: the operation of a *net.OpError ("dial", "read", ...);
//   - code and constraint: the SQLSTATE and constraint name of a *pgconn.PgError.
//
// Use as log.Error("msg", ErrAttrs(err)...).
func ErrAttrs(err error) []any {
	if err == nil {
		return nil
	}
	attrs := []any{"err_type", fmt.Sprintf("%T", err), "err_class", errClass(err)}
	var op *net.OpError
	if errors.As(err, &op) {
		attrs = append(attrs, "net_op", op.Op)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		attrs = append(attrs, "code", pg.Code)
		if pg.ConstraintName != "" {
			attrs = append(attrs, "constraint", pg.ConstraintName)
		}
	}
	return attrs
}

// errClass names the cause of err from a fixed vocabulary, or the type of the
// innermost error in the unwrap chain when nothing matches. A pool acquire
// timeout surfaces as deadline_exceeded because pgxpool returns the context
// error of the caller's Acquire.
func errClass(err error) string {
	var pg *pgconn.PgError
	var ce *pgconn.ConnectError
	var op *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.As(err, &pg):
		return "pg_error"
	case errors.As(err, &ce):
		return "pg_connect_error"
	case errors.As(err, &op):
		return "net_op_error"
	case errors.Is(err, puddle.ErrNotAvailable):
		return "pool_exhausted"
	case errors.Is(err, puddle.ErrClosedPool):
		return "pool_closed"
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}

// PanicAttrs returns slog key/value pairs for a recovered panic value. Only the
// type is recorded, never the value, which may embed sensitive data.
func PanicAttrs(v any) []any {
	return []any{"panic_type", fmt.Sprintf("%T", v)}
}
