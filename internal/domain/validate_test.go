package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

func fields(errs []domain.FieldError) map[string]bool {
	m := map[string]bool{}
	for _, e := range errs {
		m[e.Field] = true
	}
	return m
}

func TestValidateCreate(t *testing.T) {
	cases := []struct {
		name      string
		req       domain.CreateRequest
		badFields []string // empty means valid
	}{
		{"sms ok", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "hi"}, nil},
		{"sms bad recipient", domain.CreateRequest{Recipient: "05551234567", Channel: "sms", Content: "hi"}, []string{"recipient"}},
		{"sms too short number", domain.CreateRequest{Recipient: "+1234567", Channel: "sms", Content: "hi"}, []string{"recipient"}},
		{"sms 160 runes ok", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: strings.Repeat("ş", 160)}, nil},
		{"sms 161 runes", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: strings.Repeat("ş", 161)}, []string{"content"}},
		{"sms 160 emoji ok", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: strings.Repeat("😀", 160)}, nil},
		{"email ok", domain.CreateRequest{Recipient: "a@example.com", Channel: "email", Content: "x"}, nil},
		{"email display name rejected", domain.CreateRequest{Recipient: "Bob <a@example.com>", Channel: "email", Content: "x"}, []string{"recipient"}},
		{"email too long", domain.CreateRequest{Recipient: strings.Repeat("a", 250) + "@e.com", Channel: "email", Content: "x"}, []string{"recipient"}},
		{"email content 10000 ok", domain.CreateRequest{Recipient: "a@example.com", Channel: "email", Content: strings.Repeat("a", 10000)}, nil},
		{"email content 10001", domain.CreateRequest{Recipient: "a@example.com", Channel: "email", Content: strings.Repeat("a", 10001)}, []string{"content"}},
		{"push ok", domain.CreateRequest{Recipient: "tok_123", Channel: "push", Content: "x"}, nil},
		{"push token with space", domain.CreateRequest{Recipient: "tok 123", Channel: "push", Content: "x"}, []string{"recipient"}},
		{"push token 256", domain.CreateRequest{Recipient: strings.Repeat("a", 256), Channel: "push", Content: "x"}, []string{"recipient"}},
		{"push content 257", domain.CreateRequest{Recipient: "tok", Channel: "push", Content: strings.Repeat("a", 257)}, []string{"content"}},
		{"bad channel", domain.CreateRequest{Recipient: "x", Channel: "fax", Content: "x"}, []string{"channel"}},
		{"missing everything", domain.CreateRequest{}, []string{"channel", "recipient", "content"}},
		{"whitespace content", domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "   "}, []string{"content"}},
		{"bad priority", domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "x", Priority: "urgent"}, []string{"priority"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs := c.req.Validate()
			got := fields(errs)
			if len(c.badFields) == 0 && len(errs) != 0 {
				t.Fatalf("expected valid, got %v", errs)
			}
			for _, f := range c.badFields {
				if !got[f] {
					t.Errorf("expected error on %q, got %v", f, errs)
				}
			}
		})
	}
}

func TestValidateRejectsControlCharacters(t *testing.T) {
	tid := "3f0c2c0e-1111-4222-8333-444455556666"
	cases := []struct {
		name  string
		req   domain.CreateRequest
		field string // empty means valid
	}{
		{"content NUL", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "a\u0000b"}, "content"},
		{"content ESC", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "a\u001bb"}, "content"},
		{"content DEL is not C0", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "a\u007fb"}, ""},
		{"content newline and tab ok", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "a\n\tb"}, ""},
		{"push recipient NUL", domain.CreateRequest{Recipient: "tok\u0000", Channel: "push", Content: "x"}, "recipient"},
		{"email recipient NUL", domain.CreateRequest{Recipient: "a\u0000@example.com", Channel: "email", Content: "x"}, "recipient"},
		{"var value NUL", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", TemplateID: &tid, TemplateVars: map[string]string{"a": "x\u0000"}}, "template_vars"},
		{"var value newline ok", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", TemplateID: &tid, TemplateVars: map[string]string{"a": "x\ny"}}, ""},
	}
	for _, c := range cases {
		_, errs := c.req.Validate()
		if c.field == "" {
			if len(errs) != 0 {
				t.Errorf("%s: unexpected errors %v", c.name, errs)
			}
			continue
		}
		if !fields(errs)[c.field] {
			t.Errorf("%s: want error on %s, got %v", c.name, c.field, errs)
		}
		for _, e := range errs {
			if strings.Contains(e.Message, "\u0000") {
				t.Errorf("%s: message echoes the value", c.name)
			}
		}
	}
}

func TestValidateBatchControlCharacterCarriesItemIndex(t *testing.T) {
	ok := domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "hi"}
	bad := ok
	bad.Content = "hi\u0000"
	_, errs := domain.ValidateBatch(domain.BatchRequest{Notifications: []domain.CreateRequest{ok, bad}})
	if len(errs) != 1 || errs[0].Field != "content" || errs[0].Index == nil || *errs[0].Index != 1 {
		t.Fatalf("errs: %+v", errs)
	}
}

func TestHashIgnoresOmittedPriority(t *testing.T) {
	a, _ := domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "x"}.Validate()
	b, _ := domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "x", Priority: "normal"}.Validate()
	c, _ := domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "y"}.Validate()
	if a.Hash() != b.Hash() {
		t.Error("omitted priority must hash like normal")
	}
	if a.Hash() == c.Hash() {
		t.Error("different content must hash differently")
	}
}

func TestValidateBatch(t *testing.T) {
	ok := domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "x"}
	t.Run("empty", func(t *testing.T) {
		if _, errs := domain.ValidateBatch(domain.BatchRequest{}); len(errs) == 0 {
			t.Error("empty batch must fail")
		}
	})
	t.Run("1001 items", func(t *testing.T) {
		items := make([]domain.CreateRequest, 1001)
		for i := range items {
			items[i] = ok
		}
		if _, errs := domain.ValidateBatch(domain.BatchRequest{Notifications: items}); len(errs) == 0 {
			t.Error("1001 items must fail")
		}
	})
	t.Run("1000 identical items allowed", func(t *testing.T) {
		items := make([]domain.CreateRequest, 1000)
		for i := range items {
			items[i] = ok
		}
		got, errs := domain.ValidateBatch(domain.BatchRequest{Notifications: items})
		if len(errs) != 0 || len(got) != 1000 {
			t.Fatalf("errs=%v len=%d", errs, len(got))
		}
	})
	t.Run("errors carry the item index", func(t *testing.T) {
		bad := domain.CreateRequest{Recipient: "x", Channel: "fax", Content: "x"}
		_, errs := domain.ValidateBatch(domain.BatchRequest{Notifications: []domain.CreateRequest{ok, ok, bad}})
		if len(errs) == 0 || errs[0].Index == nil || *errs[0].Index != 2 {
			t.Fatalf("want index 2, got %+v", errs)
		}
	})
	t.Run("batch hash is order sensitive", func(t *testing.T) {
		a, _ := ok.Validate()
		b, _ := domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "y"}.Validate()
		if domain.BatchHash([]domain.NewNotification{a, b}) == domain.BatchHash([]domain.NewNotification{b, a}) {
			t.Error("reordered batch must hash differently")
		}
	})
}

func TestIsUUID(t *testing.T) {
	if !domain.IsUUID("123e4567-e89b-12d3-a456-426614174000") {
		t.Error("valid uuid rejected")
	}
	for _, s := range []string{"", "not-a-uuid", "123e4567e89b12d3a456426614174000", "123e4567-e89b-12d3-a456-42661417400z"} {
		if domain.IsUUID(s) {
			t.Errorf("%q must be rejected", s)
		}
	}
}

func ptr(s string) *string { return &s }

func TestScheduledAtMustBeFutureAndWithin30Days(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	tests := []struct {
		name   string
		at     *time.Time
		wantOK bool
	}{
		{"unset", nil, true},
		{"past", at(-time.Second), false},
		{"now", at(0), false},
		{"one second ahead", at(time.Second), true},
		{"exactly 30 days", at(30 * 24 * time.Hour), true},
		{"30 days plus one second", at(30*24*time.Hour + time.Second), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := domain.CheckSchedule(tc.at, now)
			if tc.wantOK != (len(errs) == 0) {
				t.Fatalf("errs = %+v", errs)
			}
			for _, e := range errs {
				if e.Field != "scheduled_at" {
					t.Errorf("field = %q", e.Field)
				}
			}
		})
	}

	t.Run("bad format is a validation error", func(t *testing.T) {
		for _, v := range []string{"tomorrow", "2026-11-23", "2026-11-23 12:00:00", ""} {
			_, errs := domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "x", ScheduledAt: ptr(v)}.Validate()
			if len(errs) != 1 || errs[0].Field != "scheduled_at" {
				t.Errorf("%q: errs = %+v", v, errs)
			}
		}
	})
	t.Run("offset is normalized to UTC", func(t *testing.T) {
		nn, errs := domain.CreateRequest{Recipient: "tok", Channel: "push", Content: "x", ScheduledAt: ptr("2026-12-01T15:00:00+03:00")}.Validate()
		if len(errs) != 0 || nn.ScheduledAt == nil {
			t.Fatalf("errs = %+v", errs)
		}
		if want := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC); !nn.ScheduledAt.Equal(want) || nn.ScheduledAt.Location() != time.UTC {
			t.Fatalf("got %v", nn.ScheduledAt)
		}
	})
	t.Run("batch index", func(t *testing.T) {
		_, errs := domain.ValidateBatch(domain.BatchRequest{Notifications: []domain.CreateRequest{
			{Recipient: "tok", Channel: "push", Content: "x"},
			{Recipient: "tok", Channel: "push", Content: "x", ScheduledAt: ptr("nope")},
		}})
		if len(errs) != 1 || errs[0].Index == nil || *errs[0].Index != 1 || errs[0].Field != "scheduled_at" {
			t.Fatalf("errs = %+v", errs)
		}
	})
}

func TestHashScheduledAtBackwardCompatible(t *testing.T) {
	// Pinned from the implementation before scheduled_at existed (PF17).
	const pinned = "c413775bd6212ec1edcad315bca953e9c5e7ae8c2b3c3b50af23bd3ce76a0f8f"
	n, errs := domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "hello", Priority: "high"}.Validate()
	if len(errs) != 0 || n.Hash() != pinned {
		t.Fatalf("hash without scheduled_at changed: %s (%v)", n.Hash(), errs)
	}
	base := domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "hello", Priority: "high"}
	a, b, c := base, base, base
	a.ScheduledAt = ptr("2026-12-01T12:00:00Z")
	b.ScheduledAt = ptr("2026-12-01T15:00:00+03:00")
	c.ScheduledAt = ptr("2026-12-01T12:00:01Z")
	na, _ := a.Validate()
	nb, _ := b.Validate()
	nc, _ := c.Validate()
	if na.Hash() != nb.Hash() {
		t.Error("same instant with different offsets must hash the same")
	}
	if na.Hash() == n.Hash() || na.Hash() == nc.Hash() {
		t.Error("scheduled_at must change the hash")
	}
}
