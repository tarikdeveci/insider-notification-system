package worker

import "github.com/tarikdeveci/insider-notification-system/internal/domain"

// ProbeSlotFree reports whether the channel's breaker would admit a row right
// now, and ends that ticket again. Test only: a leaked half open probe slot
// makes it report false.
func (e *Engine) ProbeSlotFree(ch domain.Channel) bool {
	tk, ok := e.brk[ch].admit()
	if ok {
		tk.abort()
	}
	return ok
}
