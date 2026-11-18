package engine

import (
	"context"
	"sync"
	"time"
)

// watchdog cancels attempts that stop moving bytes. The scheduler registers each
// attempt, and one goroutine scans them every WatchdogTick.
type watchdog struct {
	timeouts Timeouts

	mu       sync.Mutex
	attempts map[*watchedAttempt]struct{}
}

// watchedAttempt is one attempt's stall state. Only scan touches ticks and idle.
type watchedAttempt struct {
	activity *AttemptActivity
	cancel   context.CancelCauseFunc
	ticks    uint64
	idle     time.Duration
}

func newWatchdog(timeouts Timeouts) *watchdog {
	return &watchdog{timeouts: timeouts, attempts: make(map[*watchedAttempt]struct{})}
}

func (w *watchdog) add(activity *AttemptActivity, cancel context.CancelCauseFunc) *watchedAttempt {
	entry := &watchedAttempt{activity: activity, cancel: cancel}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attempts[entry] = struct{}{}
	return entry
}

func (w *watchdog) remove(entry *watchedAttempt) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.attempts, entry)
}

func (w *watchdog) limit(phase AttemptPhase) time.Duration {
	if phase == AttemptAwaitResponse {
		return w.timeouts.Response
	}
	return w.timeouts.Stall
}

// scan is one watchdog pass.
func (w *watchdog) scan() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for entry := range w.attempts {
		ticks, phase, parked := entry.activity.Observe()
		if parked || ticks != entry.ticks {
			entry.ticks, entry.idle = ticks, 0
			continue
		}
		entry.idle += WatchdogTick
		if entry.idle >= w.limit(phase) {
			entry.cancel(ErrStalled)
		}
	}
}

// run scans until stop closes.
func (w *watchdog) run(clock Clock, stop <-chan struct{}) {
	ticker := clock.NewTicker(WatchdogTick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			w.scan()
		case <-stop:
			return
		}
	}
}
