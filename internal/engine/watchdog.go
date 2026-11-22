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

// watchedAttempt is one attempt's stall state. Only scan touches ticks and since.
type watchedAttempt struct {
	activity *AttemptActivity
	cancel   context.CancelCauseFunc
	ticks    uint64
	// since is when scan last saw the attempt move, or zero before its first scan.
	since time.Time
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

func (w *watchdog) limit(phase AttemptPhase, activity *AttemptActivity) time.Duration {
	if phase == AttemptAwaitResponse {
		return w.timeouts.Response + activity.Drain()
	}
	return w.timeouts.Stall
}

// scan is one watchdog pass at now. An attempt's idle time is the clock time
// since a scan last saw it move, not a count of scans: ticks that arrive late
// and then bunch up (a busy or starved process) must not make a healthy attempt
// look stalled after a few milliseconds.
func (w *watchdog) scan(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for entry := range w.attempts {
		ticks, phase, parked := entry.activity.Observe()
		if parked || ticks != entry.ticks || entry.since.IsZero() {
			entry.ticks, entry.since = ticks, now
			continue
		}
		if now.Sub(entry.since) >= w.limit(phase, entry.activity) {
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
			w.scan(clock.Now())
		case <-stop:
			return
		}
	}
}
