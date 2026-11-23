package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

func TestCursorRoundTrip(t *testing.T) {
	in := store.Cursor{CreatedAt: time.UnixMicro(1700000000123456).UTC(), ID: "123e4567-e89b-12d3-a456-426614174000"}
	out, err := store.DecodeCursor(store.EncodeCursor(in))
	if err != nil || !out.CreatedAt.Equal(in.CreatedAt) || out.ID != in.ID {
		t.Fatalf("round trip: %v %+v", err, out)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	bad := []string{"", "!!!", "bm90LWEtY3Vyc29y", strings.Repeat("A", 200)}
	for _, s := range bad {
		if _, err := store.DecodeCursor(s); err == nil {
			t.Errorf("%q must be rejected", s)
		}
	}
}
