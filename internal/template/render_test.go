package template_test

import (
	"reflect"
	"runtime"
	"strings"
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

func TestRenderMaxRejectsOversizeWithoutBuilding(t *testing.T) {
	body := strings.Repeat("{{a}}", template.MaxPlaceholders)
	vars := map[string]string{"a": strings.Repeat("x", 1000)}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	text, missing, tooLong := template.RenderMax(body, vars, 160)
	runtime.ReadMemStats(&after)
	if !tooLong || text != "" || len(missing) != 0 {
		t.Fatalf("tooLong=%v len=%d missing=%v", tooLong, len(text), missing)
	}
	// The rendered text would be 100000 bytes; rejecting must not allocate it.
	if d := after.TotalAlloc - before.TotalAlloc; d > 50_000 {
		t.Fatalf("allocated %d bytes while rejecting", d)
	}
}

func TestRenderMaxCountsRunesAndAcceptsExactLimit(t *testing.T) {
	vars := map[string]string{"a": "çç"}
	if got, _, tooLong := template.RenderMax("{{a}}ab", vars, 4); tooLong || got != "ççab" {
		t.Fatalf("exact limit: %q tooLong=%v", got, tooLong)
	}
	if _, _, tooLong := template.RenderMax("{{a}}abc", vars, 4); !tooLong {
		t.Fatal("one rune over the limit must be too long")
	}
}

func TestRenderMaxReportsMissingBeforeLength(t *testing.T) {
	_, missing, tooLong := template.RenderMax("{{a}}{{b}}", map[string]string{"a": "xxxxxxxx"}, 3)
	if tooLong || !reflect.DeepEqual(missing, []string{"b"}) {
		t.Fatalf("missing=%v tooLong=%v", missing, tooLong)
	}
}

func TestParseCapsPlaceholders(t *testing.T) {
	if _, err := template.Parse(strings.Repeat("{{a}}", template.MaxPlaceholders)); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	if _, err := template.Parse(strings.Repeat("{{a}}", template.MaxPlaceholders+1)); err == nil {
		t.Fatal("over the cap must fail")
	}
}

func TestLiteralRunesIgnoresPlaceholders(t *testing.T) {
	if n := template.LiteralRunes("çab{{ name }}cd{{x}}"); n != 5 {
		t.Fatalf("got %d", n)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{"a": true, "_a": true, "A_1": true, "": false, "1a": false, "a-b": false, "a b": false, "ş": false} {
		if got := template.ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v", name, got)
		}
	}
}
