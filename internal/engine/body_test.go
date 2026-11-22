package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func newTestBody(t *testing.T, ctx context.Context, data []byte, length int64, counter *atomic.Int64) *meteredBody {
	t.Helper()
	return &meteredBody{
		ctx:         ctx,
		section:     io.NewSectionReader(bytes.NewReader(data), 0, length),
		length:      length,
		activity:    new(AttemptActivity),
		limiter:     newRateLimiter(RealClock{}, 0),
		transferred: counter,
		clock:       RealClock{},
		sendBuffer:  DefaultSendBuffer,
		connRate:    new(atomic.Int64),
	}
}

func TestMeteredBodyExposesOnlyReadAndSeek(t *testing.T) {
	var body any = &meteredBody{}
	if _, ok := body.(io.WriterTo); ok {
		t.Fatal("WriterTo would bypass metering")
	}
	if _, ok := body.(io.ReaderFrom); ok {
		t.Fatal("ReaderFrom would bypass metering")
	}
}

func TestMeteredBodyCountsAndRollsBack(t *testing.T) {
	data := make([]byte, 3*MeterQuantum)
	var counter atomic.Int64
	body := newTestBody(t, context.Background(), data, int64(len(data)), &counter)

	got, err := io.ReadAll(body)
	if err != nil || len(got) != len(data) {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
	if counter.Load() != int64(len(data)) {
		t.Fatalf("counted %d", counter.Load())
	}
	if _, _, _ = body.activity.Observe(); body.localFailure() != nil {
		t.Fatal(body.localFailure())
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if counter.Load() != 0 {
		t.Fatalf("after rewind counted %d", counter.Load())
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if published := body.retire(); published != int64(len(data)) || counter.Load() != published {
		t.Fatalf("published %d, counted %d", published, counter.Load())
	}
}

func TestMeteredBodySeekForLengthKeepsCount(t *testing.T) {
	data := make([]byte, MeterQuantum)
	var counter atomic.Int64
	body := newTestBody(t, context.Background(), data, int64(len(data)), &counter)
	if end, err := body.Seek(0, io.SeekEnd); err != nil || end != int64(len(data)) {
		t.Fatalf("end %d, %v", end, err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(body); err != nil || len(got) != len(data) {
		t.Fatalf("read %d, %v", len(got), err)
	}
}

func TestMeteredBodyReportsShortSourceAsLocalFailure(t *testing.T) {
	var counter atomic.Int64
	body := newTestBody(t, context.Background(), make([]byte, 100), 200, &counter)
	_, err := io.ReadAll(body)
	if !errors.Is(err, ErrLocalIO) || !errors.Is(body.localFailure(), ErrLocalIO) {
		t.Fatalf("read error %v, stored %v", err, body.localFailure())
	}
}

func TestMeteredBodyStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrJobPaused)
	var counter atomic.Int64
	data := make([]byte, 2*MeterQuantum)
	body := newTestBody(t, ctx, data, int64(len(data)), &counter)
	if _, err := io.ReadAll(body); !errors.Is(err, ErrJobPaused) {
		t.Fatalf("got %v", err)
	}
}

func TestRetiredBodyCannotChangeProgress(t *testing.T) {
	var counter atomic.Int64
	data := make([]byte, 2*MeterQuantum)
	body := newTestBody(t, context.Background(), data, int64(len(data)), &counter)
	body.retire()
	if _, err := io.ReadAll(body); !errors.Is(err, errBodyRetired) {
		t.Fatalf("got %v", err)
	}
	if counter.Load() != 0 {
		t.Fatalf("a late read counted %d bytes", counter.Load())
	}
}

func TestWatchdogCancelsOnlyIdleAttempts(t *testing.T) {
	timeouts := DefaultTimeouts()
	timeouts.Stall, timeouts.Response = 3*WatchdogTick, 5*WatchdogTick
	dog := newWatchdog(timeouts)

	start := func(phase AttemptPhase) (*AttemptActivity, context.Context) {
		ctx, cancel := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancel(nil) })
		activity := new(AttemptActivity)
		activity.SetPhase(phase)
		dog.add(activity, cancel)
		return activity, ctx
	}
	_, idle := start(AttemptSend)
	busy, busyCtx := start(AttemptSend)
	parked, parkedCtx := start(AttemptSend)
	parked.Park()
	_, waiting := start(AttemptAwaitResponse)

	now := time.Unix(0, 0)
	scan := func() {
		now = now.Add(WatchdogTick)
		dog.scan(now)
	}
	for range 4 {
		busy.Touch()
		scan()
	}
	if !errors.Is(context.Cause(idle), ErrStalled) {
		t.Fatalf("idle attempt: cause %v", context.Cause(idle))
	}
	if busyCtx.Err() != nil || parkedCtx.Err() != nil {
		t.Fatal("an active or parked attempt was cancelled")
	}
	if waiting.Err() != nil {
		t.Fatal("the response timeout was applied too early")
	}
	for range 2 {
		scan()
	}
	if !errors.Is(context.Cause(waiting), ErrStalled) {
		t.Fatalf("waiting attempt: cause %v", context.Cause(waiting))
	}
}

// TestWatchdogMeasuresIdleTimeNotScans: scans that come close together, as
// late ticks do on a busy machine, do not add up to a stall.
func TestWatchdogMeasuresIdleTimeNotScans(t *testing.T) {
	timeouts := DefaultTimeouts()
	timeouts.Stall = WatchdogTick
	dog := newWatchdog(timeouts)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	activity := new(AttemptActivity)
	activity.SetPhase(AttemptReceive)
	dog.add(activity, cancel)

	now := time.Unix(0, 0)
	dog.scan(now)
	for range 5 {
		now = now.Add(time.Millisecond)
		dog.scan(now)
	}
	if ctx.Err() != nil {
		t.Fatal("bunched scans stalled an attempt that was idle for 5 ms")
	}
	dog.scan(now.Add(WatchdogTick))
	if !errors.Is(context.Cause(ctx), ErrStalled) {
		t.Fatalf("a real stall was missed: cause %v", context.Cause(ctx))
	}
}

// TestPartEntryFitsBudget covers the fixed part of MemPerPartEntry. The ETag
// allocation is at most 48 bytes for S3's 34-character quoted MD5.
func TestPartEntryFitsBudget(t *testing.T) {
	const etagAllocation = 48
	if size := int64(unsafe.Sizeof(partEntry{})) + etagAllocation; size > MemPerPartEntry {
		t.Fatalf("part entry uses %d bytes, budget %d", size, MemPerPartEntry)
	}
}
