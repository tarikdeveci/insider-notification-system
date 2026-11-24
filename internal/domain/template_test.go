package domain_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

const tplID = "3f2b8c1e-5a4d-4c6e-9b7a-1d2e3f4a5b6c"

func tplReq(vars map[string]string) domain.CreateRequest {
	id := tplID
	return domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", TemplateID: &id, TemplateVars: vars}
}

func varsOf(n int, valueRunes int) map[string]string {
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("k%d", i)] = strings.Repeat("ş", valueRunes)
	}
	return m
}

func TestTemplateVarsBounds(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
		ok   bool
	}{
		{"none", nil, true},
		{"50 keys ok", varsOf(50, 1), true},
		{"51 keys rejected", varsOf(51, 1), false},
		{"1000 rune value ok", map[string]string{"a": strings.Repeat("ş", 1000)}, true},
		{"1001 rune value rejected", map[string]string{"a": strings.Repeat("ş", 1001)}, false},
		{"invalid key rejected", map[string]string{"a-b": "x"}, false},
		{"empty key rejected", map[string]string{"": "x"}, false},
	}
	for _, c := range cases {
		_, errs := tplReq(c.vars).Validate()
		if c.ok && len(errs) != 0 {
			t.Errorf("%s: unexpected errors %v", c.name, errs)
		}
		if !c.ok && !fields(errs)["template_vars"] {
			t.Errorf("%s: want template_vars error, got %v", c.name, errs)
		}
		for _, e := range errs {
			if strings.Contains(e.Message, "ş") {
				t.Errorf("%s: message echoes a value: %q", c.name, e.Message)
			}
		}
	}
}

func TestContentAndTemplateIDMutuallyExclusive(t *testing.T) {
	id := tplID
	bad := "not-a-uuid"
	cases := []struct {
		name  string
		req   domain.CreateRequest
		field string // empty means valid
	}{
		{"both", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "hi", TemplateID: &id}, "template_id"},
		{"neither", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms"}, "content"},
		{"vars without id", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "hi", TemplateVars: map[string]string{"a": "b"}}, "template_vars"},
		{"empty vars without id", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", Content: "hi", TemplateVars: map[string]string{}}, "template_vars"},
		{"template only", tplReq(map[string]string{"a": "b"}), ""},
		{"malformed id", domain.CreateRequest{Recipient: "+905551234567", Channel: "sms", TemplateID: &bad}, "template_id"},
	}
	for _, c := range cases {
		n, errs := c.req.Validate()
		if c.field == "" {
			if len(errs) != 0 || n.TemplateID == nil || *n.TemplateID != tplID || n.Content != "" {
				t.Errorf("%s: n=%+v errs=%v", c.name, n, errs)
			}
			continue
		}
		if !fields(errs)[c.field] {
			t.Errorf("%s: want error on %s, got %v", c.name, c.field, errs)
		}
	}
}

func TestTemplateIDIsNormalizedToLowercase(t *testing.T) {
	up := strings.ToUpper(tplID)
	r := tplReq(nil)
	r.TemplateID = &up
	n, errs := r.Validate()
	if len(errs) != 0 || *n.TemplateID != tplID {
		t.Fatalf("n=%+v errs=%v", n, errs)
	}
}

func TestHashCoversTemplateIDAndVars(t *testing.T) {
	a, _ := tplReq(map[string]string{"a": "1", "b": "2"}).Validate()
	same, _ := tplReq(map[string]string{"b": "2", "a": "1"}).Validate()
	diffVar, _ := tplReq(map[string]string{"a": "1", "b": "3"}).Validate()
	other := "3f2b8c1e-5a4d-4c6e-9b7a-1d2e3f4a5b6d"
	r := tplReq(map[string]string{"a": "1", "b": "2"})
	r.TemplateID = &other
	diffID, _ := r.Validate()
	if a.Hash() != same.Hash() {
		t.Error("map order must not change the hash")
	}
	if a.Hash() == diffVar.Hash() || a.Hash() == diffID.Hash() {
		t.Error("template id and vars must change the hash")
	}
}

func TestCheckRenderedContent(t *testing.T) {
	if errs := domain.CheckRenderedContent(domain.ChannelSMS, strings.Repeat("ş", 160)); len(errs) != 0 {
		t.Errorf("160 runes: %v", errs)
	}
	errs := domain.CheckRenderedContent(domain.ChannelSMS, strings.Repeat("ş", 161))
	if len(errs) != 1 || errs[0].Field != "content" || !strings.HasPrefix(errs[0].Message, "rendered content: ") {
		t.Errorf("161 runes: %v", errs)
	}
	errs = domain.CheckRenderedContent(domain.ChannelPush, "  ")
	if len(errs) != 1 || errs[0].Field != "content" || !strings.HasPrefix(errs[0].Message, "rendered content: ") {
		t.Errorf("blank: %v", errs)
	}
	if errs := domain.CheckRenderedContent(domain.ChannelEmail, strings.Repeat("a", 10000)); len(errs) != 0 {
		t.Errorf("email 10000: %v", errs)
	}
}
