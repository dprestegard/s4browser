package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"golang.org/x/time/rate"
)

// rateLimiter is the BandwidthLimiter for one transfer direction, shared by all
// of that direction's jobs. A waiter reserves bytes, then sleeps on the Clock. A
// rate change wakes every waiter so that it reserves again at the new rate.
type rateLimiter struct {
	clock Clock

	// limited lets the data path skip all work when no limit is set.
	limited atomic.Bool

	mu      sync.Mutex
	limiter *rate.Limiter // nil when unlimited
	changed chan struct{} // closed and replaced by every SetRate
}

var _ BandwidthLimiter = (*rateLimiter)(nil)

// NewBandwidthLimiter returns a limiter of bytesPerSec, where 0 means unlimited.
func NewBandwidthLimiter(clock Clock, bytesPerSec int64) BandwidthLimiter {
	return newRateLimiter(clock, bytesPerSec)
}

func newRateLimiter(clock Clock, bytesPerSec int64) *rateLimiter {
	l := &rateLimiter{clock: clock, changed: make(chan struct{})}
	l.SetRate(bytesPerSec)
	return l
}

// SetRate implements BandwidthLimiter. The burst is max(MeterQuantum, rate/10).
func (l *rateLimiter) SetRate(bytesPerSec int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bytesPerSec <= 0 {
		l.limiter = nil
	} else {
		l.limiter = rate.NewLimiter(rate.Limit(bytesPerSec), int(max(MeterQuantum, bytesPerSec/10)))
	}
	l.limited.Store(l.limiter != nil)
	close(l.changed)
	l.changed = make(chan struct{})
}

// WaitN implements BandwidthLimiter. It returns the cause of ctx when ctx ends.
func (l *rateLimiter) WaitN(ctx context.Context, n int) error {
	for n > 0 {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		l.mu.Lock()
		limiter, changed := l.limiter, l.changed
		if limiter == nil {
			l.mu.Unlock()
			return nil
		}
		now := l.clock.Now()
		chunk := min(n, limiter.Burst())
		reservation := limiter.ReserveN(now, chunk)
		l.mu.Unlock()

		delay := reservation.DelayFrom(now)
		if delay <= 0 {
			n -= chunk
			continue
		}
		timer := l.clock.NewTimer(delay)
		select {
		case <-timer.C():
			n -= chunk
		case <-changed:
			// The reservation belongs to the replaced limiter, so it is simply dropped.
		case <-ctx.Done():
			timer.Stop()
			reservation.CancelAt(l.clock.Now())
			return context.Cause(ctx)
		}
		timer.Stop()
	}
	return nil
}
