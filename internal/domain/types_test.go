package domain_test

import (
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

func TestParsePriority(t *testing.T) {
	cases := map[string]struct {
		want domain.Priority
		ok   bool
	}{
		"": {domain.PriorityNormal, true}, "high": {domain.PriorityHigh, true},
		"normal": {domain.PriorityNormal, true}, "low": {domain.PriorityLow, true},
		"urgent": {0, false}, "HIGH": {0, false},
	}
	for in, c := range cases {
		got, ok := domain.ParsePriority(in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParsePriority(%q) = %v,%v", in, got, ok)
		}
	}
	if domain.PriorityHigh.String() != "high" || domain.PriorityLow.String() != "low" {
		t.Error("String mismatch")
	}
}

func TestCanTransition(t *testing.T) {
	ok := [][2]domain.Status{
		{domain.StatusPending, domain.StatusProcessing}, {domain.StatusPending, domain.StatusCancelled},
		{domain.StatusProcessing, domain.StatusSent}, {domain.StatusProcessing, domain.StatusPending},
		{domain.StatusProcessing, domain.StatusFailed}, {domain.StatusFailed, domain.StatusPending},
	}
	for _, p := range ok {
		if !domain.CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be allowed", p[0], p[1])
		}
	}
	bad := [][2]domain.Status{
		{domain.StatusProcessing, domain.StatusCancelled}, {domain.StatusSent, domain.StatusPending},
		{domain.StatusCancelled, domain.StatusPending}, {domain.StatusFailed, domain.StatusSent},
		{domain.StatusPending, domain.StatusSent},
	}
	for _, p := range bad {
		if domain.CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be rejected", p[0], p[1])
		}
	}
}
