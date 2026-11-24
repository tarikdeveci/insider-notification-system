// Package template parses and renders notification templates.
//
// The grammar is deliberately tiny: a placeholder is "{{", optional spaces, a
// name matching [A-Za-z_][A-Za-z0-9_]*, optional spaces, "}}". There is no
// escaping, no filter and no nesting. Rendering is a single pass, so a
// substituted value is never scanned again for placeholders.
package template

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ValidName reports whether s is a legal placeholder or variable name.
func ValidName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// segment is a literal text run or a placeholder name.
type segment struct {
	text  string
	name  string // set for a placeholder
	isVar bool
}

// scan splits body into literal and placeholder segments.
func scan(body string) ([]segment, error) {
	var segs []segment
	for {
		i := strings.Index(body, "{{")
		if i < 0 {
			if body != "" {
				segs = append(segs, segment{text: body})
			}
			return segs, nil
		}
		if i > 0 {
			segs = append(segs, segment{text: body[:i]})
		}
		rest := body[i+2:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return nil, fmt.Errorf("unclosed placeholder")
		}
		name := strings.Trim(rest[:j], " ")
		if !ValidName(name) {
			return nil, fmt.Errorf("invalid placeholder name")
		}
		segs = append(segs, segment{name: name, isVar: true})
		body = rest[j+2:]
	}
}

// Parse returns the distinct placeholder names of body in order of first use.
// It fails on an unclosed "{{" or an invalid name. The error never echoes the
// template text.
func Parse(body string) ([]string, error) {
	segs, err := scan(body)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	names := []string{}
	count := 0
	for _, s := range segs {
		if !s.isVar {
			continue
		}
		if count++; count > MaxPlaceholders {
			return nil, fmt.Errorf("too many placeholders (at most %d)", MaxPlaceholders)
		}
		if !seen[s.name] {
			seen[s.name] = true
			names = append(names, s.name)
		}
	}
	return names, nil
}

// MaxPlaceholders caps the placeholder occurrences of one template body.
const MaxPlaceholders = 100

// LiteralRunes returns the rune count of the literal text of body, that is
// the part of every rendering that no variable can shrink. A body that does
// not parse counts as 0.
func LiteralRunes(body string) int {
	segs, err := scan(body)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range segs {
		if !s.isVar {
			n += utf8.RuneCountInString(s.text)
		}
	}
	return n
}

// Render substitutes vars into body and returns the text plus the sorted,
// distinct names that had no value. Extra vars are ignored. A body that does
// not parse is rendered as empty with no missing names; callers validate with
// Parse when a template is created, so stored bodies always parse.
func Render(body string, vars map[string]string) (string, []string) {
	text, missing, _ := RenderMax(body, vars, -1)
	return text, missing
}

// RenderMax is Render with a size guard. It first computes the rendered rune
// count from the segments and the variable lengths and, when that exceeds
// maxRunes (maxRunes < 0 disables the guard), returns tooLong without building
// the text. Missing names are reported before the length is considered.
func RenderMax(body string, vars map[string]string, maxRunes int) (text string, missing []string, tooLong bool) {
	segs, err := scan(body)
	if err != nil {
		return "", nil, false
	}
	seen := map[string]bool{}
	runes, bytes := 0, 0
	varRunes := map[string]int{}
	for _, s := range segs {
		if !s.isVar {
			runes += utf8.RuneCountInString(s.text)
			bytes += len(s.text)
			continue
		}
		v, ok := vars[s.name]
		if !ok {
			if !seen[s.name] {
				seen[s.name] = true
				missing = append(missing, s.name)
			}
			continue
		}
		n, cached := varRunes[s.name]
		if !cached {
			n = utf8.RuneCountInString(v)
			varRunes[s.name] = n
		}
		runes += n
		bytes += len(v)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", missing, false
	}
	if maxRunes >= 0 && runes > maxRunes {
		return "", nil, true
	}
	var b strings.Builder
	b.Grow(bytes)
	for _, s := range segs {
		if s.isVar {
			b.WriteString(vars[s.name])
		} else {
			b.WriteString(s.text)
		}
	}
	return b.String(), nil, false
}
