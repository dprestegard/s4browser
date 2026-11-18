package fakes3

import (
	"sort"
	"sync"
	"time"

	"s4browser/internal/engine"
)

// FakeClock is an engine.Clock whose time moves only when a test calls Advance.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeTimer
	changed chan struct{}
}

var _ engine.Clock = (*FakeClock)(nil)

// NewFakeClock returns a clock starting at start. A zero start is replaced by
// a fixed date, so results do not depend on the real time.
func NewFakeClock(start time.Time) *FakeClock {
	if start.IsZero() {
		start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return &FakeClock{now: start, changed: make(chan struct{})}
}

// Now implements engine.Clock.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

type fakeTimer struct {
	clock  *FakeClock
	ch     chan time.Time
	when   time.Time
	period time.Duration // > 0 for tickers
	active bool
}

func (c *FakeClock) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *FakeClock) add(d, period time.Duration) *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, ch: make(chan time.Time, 1), when: c.now.Add(d), period: period, active: true}
	c.waiters = append(c.waiters, t)
	c.signalLocked()
	return t
}

// NewTimer implements engine.Clock.
func (c *FakeClock) NewTimer(d time.Duration) engine.Timer { return c.add(d, 0) }

// NewTicker implements engine.Clock. d must be positive.
func (c *FakeClock) NewTicker(d time.Duration) engine.Ticker { return tickerView{c.add(d, d)} }

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	t.clock.signalLocked()
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	was := t.active
	t.when = c.now.Add(d)
	if !t.active {
		t.active = true
		c.waiters = append(c.waiters, t)
	}
	c.signalLocked()
	return was
}

type tickerView struct{ t *fakeTimer }

func (v tickerView) C() <-chan time.Time { return v.t.ch }
func (v tickerView) Stop()               { v.t.Stop() }

// Pending returns the number of active timers and tickers.
func (c *FakeClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingLocked()
}

func (c *FakeClock) pendingLocked() int {
	n := 0
	for _, w := range c.waiters {
		if w.active {
			n++
		}
	}
	return n
}

// WaitPending blocks until at least n timers are active, or the real timeout
// passes. It lets a test advance time only after the code under test has
// started waiting. It reports whether the condition was reached.
func (c *FakeClock) WaitPending(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		ok, changed := c.pendingLocked() >= n, c.changed
		c.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-changed:
		case <-deadline:
			return false
		}
	}
}

// Advance moves time forward by d, firing timers and tickers in time order.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	target := c.now.Add(d)
	for {
		sort.SliceStable(c.waiters, func(i, j int) bool { return c.waiters[i].when.Before(c.waiters[j].when) })
		var next *fakeTimer
		for _, w := range c.waiters {
			if w.active && !w.when.After(target) {
				next = w
				break
			}
		}
		if next == nil {
			break
		}
		c.now = next.when
		select {
		case next.ch <- c.now:
		default:
		}
		if next.period > 0 {
			next.when = next.when.Add(next.period)
		} else {
			next.active = false
		}
	}
	c.now = target
	live := c.waiters[:0]
	for _, w := range c.waiters {
		if w.active {
			live = append(live, w)
		}
	}
	c.waiters = live
	c.signalLocked()
}
