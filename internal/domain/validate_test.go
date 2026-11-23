package domain_test

import (
	"strings"
	"testing"

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
