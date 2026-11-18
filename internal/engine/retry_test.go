package engine

import (
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	p := DefaultRetryPolicy()
	tests := []struct {
		name       string
		attempt    int
		throttled  bool
		retryAfter time.Duration
		jitter     float64
		want       time.Duration
	}{
		{"zero jitter", 1, false, 0, 0, 0},
		{"first attempt half", 1, false, 0, 0.5, 250 * time.Millisecond},
		{"third attempt doubles twice", 3, false, 0, 1, 2 * time.Second},
		{"throttle base", 1, true, 0, 1, 2 * time.Second},
		{"cap", 20, false, 0, 1, 30 * time.Second},
		{"huge attempt does not overflow", 5000, true, 0, 1, 30 * time.Second},
		{"attempt zero treated as first", 0, false, 0, 1, 500 * time.Millisecond},
		{"retry-after is a floor", 1, false, 10 * time.Second, 0.5, 10 * time.Second},
		{"retry-after is capped", 1, false, time.Hour, 0, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Delay(tt.attempt, tt.throttled, tt.retryAfter, tt.jitter)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryExhaustedNeedsBothLimits(t *testing.T) {
	p := DefaultRetryPolicy()
	tests := []struct {
		name          string
		attempts      int
		sinceProgress time.Duration
		want          bool
	}{
		{"neither", 1, time.Minute, false},
		{"attempts only: outage is tolerated", p.MaxPartAttempts, time.Minute, false},
		{"silence only: attempts remain", 1, 2 * p.OutageTolerance, false},
		{"both", p.MaxPartAttempts, p.OutageTolerance, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Exhausted(tt.attempts, p.MaxPartAttempts, tt.sinceProgress); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
