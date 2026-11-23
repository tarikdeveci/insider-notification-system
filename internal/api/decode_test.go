package api

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	type dst struct {
		A string `json:"a"`
	}
	cases := []struct {
		name       string
		body       string
		max        int64
		wantStatus int // 0 means success
	}{
		{"ok", `{"a":"x"}`, 100, 0},
		{"unknown field", `{"b":1}`, 100, 400},
		{"trailing second object", `{"a":"x"} {}`, 100, 400},
		{"empty body", ``, 100, 400},
		{"null", `null`, 100, 400},
		{"array", `[]`, 100, 400},
		{"truncated", `{"a"`, 100, 400},
		{"too large", `{"a":"` + strings.Repeat("x", 200) + `"}`, 100, 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var d dst
			r := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
			err := decodeJSON(httptest.NewRecorder(), r, &d, c.max)
			if c.wantStatus == 0 {
				if err != nil || d.A != "x" {
					t.Fatalf("err=%v d=%+v", err, d)
				}
				return
			}
			var re *requestError
			if !errors.As(err, &re) || re.Status != c.wantStatus {
				t.Fatalf("want status %d, got %v", c.wantStatus, err)
			}
		})
	}
}
