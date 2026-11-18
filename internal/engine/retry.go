package engine

import "time"

// RetryPolicy is the engine's only retry policy. SDK retries are disabled.
type RetryPolicy struct {
	// BaseDelay is the first backoff ceiling for ordinary retryable errors.
	BaseDelay time.Duration
	// ThrottleBaseDelay is the first backoff ceiling after a throttling error.
	ThrottleBaseDelay time.Duration
	// MaxDelay caps every backoff, including a server's Retry-After.
	MaxDelay time.Duration
	// MaxPartAttempts is the attempt count at which a part may fail.
	MaxPartAttempts int
	// MaxControlAttempts is the same limit for Create, Complete and Head.
	MaxControlAttempts int
	// MaxAbortAttempts is the limit for AbortMultipartUpload. After it the
	// journal entry stays, and the abort is retried at next launch.
	MaxAbortAttempts int
	// OutageTolerance keeps a long network outage from killing a transfer. A
	// part or control request with no attempts left still retries until its job
	// has made no progress for this long.
	OutageTolerance time.Duration
}

// DefaultRetryPolicy returns the production policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		BaseDelay:          500 * time.Millisecond,
		ThrottleBaseDelay:  2 * time.Second,
		MaxDelay:           30 * time.Second,
		MaxPartAttempts:    10,
		MaxControlAttempts: 8,
		MaxAbortAttempts:   5,
		OutageTolerance:    30 * time.Minute,
	}
}

// Delay returns the wait before the next try, using full jitter:
//
//	ceiling = min(MaxDelay, base * 2^(attempt-1))
//	delay   = max(ceiling * jitter, min(retryAfter, MaxDelay))
//
// attempt is the 1-based number of the attempt that just failed. jitter is a
// value in [0,1) from Config.Jitter.
func (p RetryPolicy) Delay(attempt int, throttled bool, retryAfter time.Duration, jitter float64) time.Duration {
	base := p.BaseDelay
	if throttled {
		base = p.ThrottleBaseDelay
	}
	ceiling := p.MaxDelay
	if shift := max(attempt-1, 0); shift < 32 && base<<shift < p.MaxDelay {
		ceiling = base << shift
	}
	return max(time.Duration(float64(ceiling)*jitter), min(retryAfter, p.MaxDelay))
}

// Exhausted reports whether a retryable failure must now fail the job. Both
// limits must be reached. attempts counts the failures so far, limit is
// MaxPartAttempts or MaxControlAttempts, and sinceProgress is the time since
// the job last finished a part or entered JobRunning.
func (p RetryPolicy) Exhausted(attempts, limit int, sinceProgress time.Duration) bool {
	return attempts >= limit && sinceProgress >= p.OutageTolerance
}
