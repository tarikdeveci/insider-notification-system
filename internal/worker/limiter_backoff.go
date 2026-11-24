package worker

import (
	"sync"
	"time"
)

// limiterBackoff paces one channel loop while its rate limiter keeps failing
// (for example Redis is down). Without it every claimed row would be released
// at once and claimed again, a hot loop against the database. The pause doubles
// per failed wave up to max and ends with the first admitted row.
//
// Rows of one claim fail together; a failure inside the current pause belongs to
// the same wave and does not double it again.
type limiterBackoff struct {
	mu       sync.Mutex
	min, max time.Duration
	cur      time.Duration // 0 while the limiter is healthy
	until    time.Time
}

func newLimiterBackoff(min, max time.Duration) *limiterBackoff {
	return &limiterBackoff{min: min, max: max}
}

// fail records a limiter error. It returns how long the failed row should stay
// out of the queue and whether this error starts a new outage (the caller logs
// that one at Warn and the rest at Debug).
func (b *limiterBackoff) fail(now time.Time) (delay time.Duration, first bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.cur == 0:
		b.cur, first = b.min, true
	case now.Before(b.until):
		return b.until.Sub(now), false
	default:
		b.cur = min(b.cur*2, b.max)
	}
	b.until = now.Add(b.cur)
	return b.cur, first
}

// ok records an admitted row: the limiter works again.
func (b *limiterBackoff) ok() {
	b.mu.Lock()
	b.cur = 0
	b.mu.Unlock()
}

// remaining is how long the loop should still hold off claiming.
func (b *limiterBackoff) remaining(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == 0 || !now.Before(b.until) {
		return 0
	}
	return b.until.Sub(now)
}
