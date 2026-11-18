package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
)

func waitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("WaitN did not return")
		return nil
	}
}

func TestLimiterUnlimitedNeverWaits(t *testing.T) {
	limiter := engine.NewBandwidthLimiter(fakes3.NewFakeClock(time.Time{}), 0)
	if err := limiter.WaitN(context.Background(), 1<<40); err != nil {
		t.Fatal(err)
	}
}

func TestLimiterEnforcesRateOnFakeClock(t *testing.T) {
	clock := fakes3.NewFakeClock(time.Time{})
	const rate = 10 << 20
	limiter := engine.NewBandwidthLimiter(clock, rate)
	ctx := context.Background()
	// The first burst (rate/10) passes at once.
	if err := limiter.WaitN(ctx, rate/10); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- limiter.WaitN(ctx, rate/10) }()
	if !clock.WaitPending(1, 5*time.Second) {
		t.Fatal("no timer started")
	}
	clock.Advance(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("returned after half the required time: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	clock.Advance(60 * time.Millisecond)
	if err := waitResult(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestLimiterSplitsRequestsLargerThanBurst(t *testing.T) {
	clock := fakes3.NewFakeClock(time.Time{})
	const rate = 10 << 20
	limiter := engine.NewBandwidthLimiter(clock, rate)
	done := make(chan error, 1)
	go func() { done <- limiter.WaitN(context.Background(), 3*rate) }()
	for range 400 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
		clock.WaitPending(1, 20*time.Millisecond)
		clock.Advance(10 * time.Millisecond)
	}
	t.Fatal("a request above the burst never finished")
}

func TestLimiterRateChangeWakesWaiters(t *testing.T) {
	for name, newRate := range map[string]int64{"raised": 1 << 40, "removed": 0} {
		t.Run(name, func(t *testing.T) {
			clock := fakes3.NewFakeClock(time.Time{})
			limiter := engine.NewBandwidthLimiter(clock, 1<<20)
			ctx := context.Background()
			if err := limiter.WaitN(ctx, 256<<10); err != nil { // Use up the burst.
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- limiter.WaitN(ctx, 256<<10) }()
			if !clock.WaitPending(1, 5*time.Second) {
				t.Fatal("no timer started")
			}
			limiter.SetRate(newRate) // The clock never advances.
			if err := waitResult(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLimiterLoweredRateAppliesToWaiters(t *testing.T) {
	clock := fakes3.NewFakeClock(time.Time{})
	limiter := engine.NewBandwidthLimiter(clock, 10<<20)
	ctx := context.Background()
	if err := limiter.WaitN(ctx, 1<<20); err != nil {
		t.Fatal(err)
	}
	limiter.SetRate(256 << 10)
	if err := limiter.WaitN(ctx, 256<<10); err != nil { // The new, smaller burst.
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- limiter.WaitN(ctx, 256<<10) }()
	if !clock.WaitPending(1, 5*time.Second) {
		t.Fatal("no timer started")
	}
	clock.Advance(500 * time.Millisecond) // Enough at the old rate, half at the new one.
	select {
	case err := <-done:
		t.Fatalf("the old rate still applies: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	clock.Advance(600 * time.Millisecond)
	if err := waitResult(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestLimiterReturnsContextCause(t *testing.T) {
	clock := fakes3.NewFakeClock(time.Time{})
	limiter := engine.NewBandwidthLimiter(clock, 1<<20)
	ctx, cancel := context.WithCancelCause(context.Background())
	if err := limiter.WaitN(ctx, 256<<10); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- limiter.WaitN(ctx, 256<<10) }()
	if !clock.WaitPending(1, 5*time.Second) {
		t.Fatal("no timer started")
	}
	cancel(engine.ErrJobPaused)
	if err := waitResult(t, done); !errors.Is(err, engine.ErrJobPaused) {
		t.Fatalf("got %v", err)
	}
}
