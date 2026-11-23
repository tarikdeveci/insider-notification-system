package observability

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrAttrs returns slog key/value pairs describing err without ever including
// err.Error(), which can carry row data (recipient, content) from Postgres or
// wrapped errors. It records err_type (%T) and, for a *pgconn.PgError, the
// SQLSTATE code and the constraint name. Use as log.Error("msg", ErrAttrs(err)...).
func ErrAttrs(err error) []any {
	if err == nil {
		return nil
	}
	attrs := []any{"err_type", fmt.Sprintf("%T", err)}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		attrs = append(attrs, "code", pg.Code)
		if pg.ConstraintName != "" {
			attrs = append(attrs, "constraint", pg.ConstraintName)
		}
	}
	return attrs
}

// PanicAttrs returns slog key/value pairs for a recovered panic value. Only the
// type is recorded, never the value, which may embed sensitive data.
func PanicAttrs(v any) []any {
	return []any{"panic_type", fmt.Sprintf("%T", v)}
}
