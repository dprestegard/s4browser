package engine

import (
	"sync/atomic"
	"time"
)

// AttemptPhase says what an in-flight attempt is waiting for, which selects the
// watchdog's timeout.
type AttemptPhase uint32

const (
	// AttemptAwaitResponse covers connecting, 100-continue and waiting for the
	// response after the body is sent. Timeouts.Response applies.
	AttemptAwaitResponse AttemptPhase = iota
	// AttemptSend is reading the request body. Timeouts.Stall applies.
	AttemptSend
	// AttemptReceive is reading the response body. Timeouts.Stall applies.
	AttemptReceive
)

// AttemptActivity is the shared state between one attempt's data path and the
// watchdog. The data path calls Touch once per Read, not per byte, and never
// reads the clock. The watchdog compares Ticks between scans.
type AttemptActivity struct {
	ticks  atomic.Uint64
	phase  atomic.Uint32
	parked atomic.Int32
	drain  atomic.Int64
}

// Touch records that bytes moved.
func (a *AttemptActivity) Touch() { a.ticks.Add(1) }

// SetPhase records a phase change and counts as activity.
func (a *AttemptActivity) SetPhase(p AttemptPhase) {
	a.phase.Store(uint32(p))
	a.ticks.Add(1)
}

// Park suspends stall detection while the attempt deliberately waits, on the
// bandwidth limiter or the download lead gate.
func (a *AttemptActivity) Park() { a.parked.Add(1) }

// Unpark resumes stall detection and counts as activity.
func (a *AttemptActivity) Unpark() {
	a.parked.Add(-1)
	a.ticks.Add(1)
}

// SetDrain records how long the bytes still in the socket may take to leave
// once the body has been read. The watchdog adds it to Timeouts.Response, so a
// slow link is not taken for a stalled one while its send buffer empties.
func (a *AttemptActivity) SetDrain(d time.Duration) { a.drain.Store(int64(d)) }

// Drain returns the value set by SetDrain.
func (a *AttemptActivity) Drain() time.Duration { return time.Duration(a.drain.Load()) }

// Observe returns the state read by the watchdog.
func (a *AttemptActivity) Observe() (ticks uint64, phase AttemptPhase, parked bool) {
	return a.ticks.Load(), AttemptPhase(a.phase.Load()), a.parked.Load() > 0
}
