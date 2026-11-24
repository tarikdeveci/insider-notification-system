package worker

import (
	"testing"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/provider"
)

func fixed(v float64) func() float64 { return func() float64 { return v } }

func TestBackoffFullJitterBounds(t *testing.T) {
	p := DefaultPolicy()
	tests := []struct {
		attempt int
		rnd     float64
		want    time.Duration
	}{
		{1, 0, 0},
		{1, 0.5, time.Second},
		{2, 0.5, 2 * time.Second},
		{3, 0.5, 4 * time.Second},
		{4, 0.999, time.Duration(0.999 * float64(16*time.Second))},
		{0, 0.5, time.Second},
		{-5, 0.5, time.Second},
	}
	for _, tt := range tests {
		if got := p.Backoff(tt.attempt, fixed(tt.rnd)); got != tt.want {
			t.Errorf("attempt %d rnd %v: got %v want %v", tt.attempt, tt.rnd, got, tt.want)
		}
	}
	for attempt := 1; attempt <= 12; attempt++ {
		ceil := min(p.Cap, p.Base<<(attempt-1))
		for _, r := range []float64{0, 0.25, 0.5, 0.999999} {
			got := p.Backoff(attempt, fixed(r))
			if got < 0 || got > ceil {
				t.Errorf("attempt %d rnd %v: %v outside [0,%v]", attempt, r, got, ceil)
			}
		}
	}
}

func TestBackoffCapsAtTenMinutes(t *testing.T) {
	p := DefaultPolicy()
	for _, attempt := range []int{10, 20, 62, 63, 64, 100, 1 << 30} {
		if got := p.Backoff(attempt, fixed(1)); got != 10*time.Minute {
			t.Errorf("attempt %d: got %v want 10m", attempt, got)
		}
		if got := p.Backoff(attempt, fixed(0.5)); got != 5*time.Minute {
			t.Errorf("attempt %d half: got %v want 5m", attempt, got)
		}
	}
}

func TestDecisionTable(t *testing.T) {
	p := DefaultPolicy()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Minute)
	base := Input{Attempts: 1, MaxAttempts: 5, CreatedAt: created, Now: now}
	with := func(res provider.Result, attempts int) Input {
		in := base
		in.Result = res
		in.Attempts = attempts
		return in
	}
	tests := []struct {
		name string
		in   Input
		want Outcome
	}{
		{"success", with(provider.Result{Class: provider.Success}, 1), Outcome{Action: ActionSent}},
		{"throttled with retry-after", with(provider.Result{Class: provider.Throttled, RetryAfter: 7 * time.Second}, 5),
			Outcome{Action: ActionRetry, Delay: 7 * time.Second, GiveBackAttempt: true}},
		{"throttled no retry-after uses backoff", with(provider.Result{Class: provider.Throttled}, 3),
			Outcome{Action: ActionRetry, Delay: 2 * time.Second, GiveBackAttempt: true}},
		{"throttled at max attempts never fails", with(provider.Result{Class: provider.Throttled, RetryAfter: time.Second}, 99),
			Outcome{Action: ActionRetry, Delay: time.Second, GiveBackAttempt: true}},
		{"retryable under max", with(provider.Result{Class: provider.Retryable}, 2),
			Outcome{Action: ActionRetry, Delay: 1 * time.Second}},
		{"retryable at max", with(provider.Result{Class: provider.Retryable}, 5),
			Outcome{Action: ActionFail, Reason: ReasonExhausted}},
		{"permanent", with(provider.Result{Class: provider.Permanent}, 1),
			Outcome{Action: ActionFail, Reason: ReasonPermanent}},
	}
	for _, tt := range tests {
		got := p.Decide(tt.in, fixed(0.25))
		want := tt.want
		switch tt.name {
		case "throttled no retry-after uses backoff":
			want.Delay = 2 * time.Second // 0.25 * 8s
		case "retryable under max":
			want.Delay = 1 * time.Second // 0.25 * 4s
		}
		if got != want {
			t.Errorf("%s: got %+v want %+v", tt.name, got, want)
		}
	}
}

func TestMaxDeliveryAgeExpires(t *testing.T) {
	p := DefaultPolicy()
	created := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	sched := created.Add(10 * time.Hour)
	tests := []struct {
		name      string
		sched     *time.Time
		now       time.Time
		class     provider.Class
		wantFail  bool
		wantGiven bool
	}{
		{"created origin just before", nil, created.Add(24*time.Hour - time.Nanosecond), provider.Retryable, false, false},
		{"created origin at bound", nil, created.Add(24 * time.Hour), provider.Retryable, true, false},
		{"scheduled origin wins", &sched, created.Add(24 * time.Hour), provider.Retryable, false, false},
		{"scheduled origin at bound", &sched, sched.Add(24 * time.Hour), provider.Retryable, true, false},
		{"throttled expires too", &sched, sched.Add(25 * time.Hour), provider.Throttled, true, false},
	}
	for _, tt := range tests {
		in := Input{
			Result: provider.Result{Class: tt.class, RetryAfter: time.Second}, Attempts: 1, MaxAttempts: 5,
			CreatedAt: created, ScheduledAt: tt.sched, Now: tt.now,
		}
		got := p.Decide(in, fixed(0.5))
		if tt.wantFail {
			if got.Action != ActionFail || got.Reason != ReasonExpired || got.GiveBackAttempt {
				t.Errorf("%s: got %+v want expired fail", tt.name, got)
			}
		} else if got.Action != ActionRetry {
			t.Errorf("%s: got %+v want retry", tt.name, got)
		}
	}
	// A manual requeue restarts the window from requeued_at.
	requeued := created.Add(47 * time.Hour)
	in := Input{Result: provider.Result{Class: provider.Retryable}, Attempts: 1, MaxAttempts: 5,
		CreatedAt: created, RequeuedAt: &requeued, Now: created.Add(48 * time.Hour)}
	if got := p.Decide(in, fixed(0.5)); got.Action != ActionRetry {
		t.Errorf("requeued origin: got %+v want retry", got)
	}
	in.Now = requeued.Add(24 * time.Hour)
	if got := p.Decide(in, fixed(0.5)); got.Reason != ReasonExpired {
		t.Errorf("requeued origin at bound: got %+v want expired", got)
	}
	// Success and permanent are not subject to the age check.
	late := Input{Attempts: 1, MaxAttempts: 5, CreatedAt: created, Now: created.Add(48 * time.Hour)}
	late.Result = provider.Result{Class: provider.Success}
	if got := p.Decide(late, fixed(0)); got.Action != ActionSent {
		t.Errorf("late success: %+v", got)
	}
	late.Result = provider.Result{Class: provider.Permanent}
	if got := p.Decide(late, fixed(0)); got.Reason != ReasonPermanent {
		t.Errorf("late permanent: %+v", got)
	}
}

func TestClockAndJitterInjectable(t *testing.T) {
	p := Policy{Base: time.Second, Cap: time.Minute, MaxAge: time.Hour}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	in := Input{Result: provider.Result{Class: provider.Retryable}, Attempts: 3, MaxAttempts: 5, CreatedAt: now, Now: now}
	calls := 0
	rnd := func() float64 { calls++; return 0.5 }
	got := p.Decide(in, rnd)
	if want := 2 * time.Second; got.Delay != want {
		t.Errorf("got %v want %v", got.Delay, want)
	}
	if calls != 1 {
		t.Errorf("rnd called %d times, want 1", calls)
	}
	if p.Backoff(30, fixed(1)) != time.Minute {
		t.Error("custom cap not honoured")
	}
}
