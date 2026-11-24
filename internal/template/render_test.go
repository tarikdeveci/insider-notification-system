package template_test

import (
	"reflect"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/template"
)

func TestRenderSubstitutesVariables(t *testing.T) {
	cases := []struct {
		name, body string
		vars       map[string]string
		want       string
		missing    []string
	}{
		{"simple", "Hi {{name}}", map[string]string{"name": "Ada"}, "Hi Ada", nil},
		{"spaces inside braces", "Hi {{  name }}!", map[string]string{"name": "Ada"}, "Hi Ada!", nil},
		{"repeated", "{{a}}-{{a}}-{{b}}", map[string]string{"a": "1", "b": "2"}, "1-1-2", nil},
		{"extra vars ignored", "Hi {{a}}", map[string]string{"a": "x", "unused": "y"}, "Hi x", nil},
		{"value stays literal", "{{a}} {{b}}", map[string]string{"a": "{{b}}", "b": "B"}, "{{b}} B", nil},
		{"no placeholders", "plain", nil, "plain", nil},
		{"single brace is text", "{a} }}", nil, "{a} }}", nil},
		{"missing sorted and distinct", "{{z}} {{a}} {{z}} {{m}}", map[string]string{"m": "1"}, "1", []string{"a", "z"}},
		{"unicode text", "Merhaba {{ad}} ş", map[string]string{"ad": "Çağrı"}, "Merhaba Çağrı ş", nil},
	}
	for _, c := range cases {
		got, missing := template.Render(c.body, c.vars)
		if len(c.missing) == 0 {
			if got != c.want || len(missing) != 0 {
				t.Errorf("%s: got %q missing %v", c.name, got, missing)
			}
			continue
		}
		if !reflect.DeepEqual(missing, c.missing) {
			t.Errorf("%s: missing %v, want %v", c.name, missing, c.missing)
		}
	}
}

func TestParseNamesInOrderOfFirstUse(t *testing.T) {
	got, err := template.Parse("{{b}} {{ a }} {{b}} {{_c1}}")
	if err != nil || !reflect.DeepEqual(got, []string{"b", "a", "_c1"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := template.Parse("none"); err != nil || len(got) != 0 {
		t.Fatalf("no placeholders: %v, %v", got, err)
	}
}

func TestParseRejectsMalformedPlaceholders(t *testing.T) {
	for _, body := range []string{"{{", "hi {{name", "{{ name }", "{{}}", "{{ }}", "{{1a}}", "{{a-b}}", "{{a b}}", "{{.a}}", "{{a|f}}"} {
		if _, err := template.Parse(body); err == nil {
			t.Errorf("%q must be rejected", body)
		}
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{"a": true, "_a": true, "A_1": true, "": false, "1a": false, "a-b": false, "a b": false, "ş": false} {
		if got := template.ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v", name, got)
		}
	}
}
