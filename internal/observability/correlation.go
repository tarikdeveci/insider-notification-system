// Package observability holds logging and request correlation helpers.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

type ctxKey struct{}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// WithCorrelationID returns a context carrying the correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// CorrelationID returns the id stored by WithCorrelationID, or "" when absent.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// ValidRequestID reports whether s is acceptable as a client supplied request id.
func ValidRequestID(s string) bool { return requestIDPattern.MatchString(s) }

// NewRequestID returns 16 random bytes as 32 hex characters.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("observability: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
