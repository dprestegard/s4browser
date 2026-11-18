package engine

import "time"

// RealClock is the production Clock.
type RealClock struct{}

var _ Clock = RealClock{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// NewTimer implements Clock.
func (RealClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

// NewTicker implements Clock.
func (RealClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time        { return t.timer.C }
func (t realTimer) Stop() bool                 { return t.timer.Stop() }
func (t realTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }

type realTicker struct{ ticker *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.ticker.C }
func (t realTicker) Stop()               { t.ticker.Stop() }
