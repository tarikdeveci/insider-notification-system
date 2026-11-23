package store

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

// Cursor is the keyset position of the last row of a page.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ErrBadCursor is returned for any malformed cursor string.
var ErrBadCursor = errors.New("invalid cursor")

const maxCursorLen = 128

// EncodeCursor renders a cursor as base64url of "<unix micro>|<uuid>".
func EncodeCursor(c Cursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixMicro(), 10) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a string produced by EncodeCursor.
func DecodeCursor(s string) (Cursor, error) {
	if len(s) == 0 || len(s) > maxCursorLen {
		return Cursor{}, ErrBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 2 {
		return Cursor{}, ErrBadCursor
	}
	micro, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || !domain.IsUUID(parts[1]) {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{CreatedAt: time.UnixMicro(micro).UTC(), ID: parts[1]}, nil
}
