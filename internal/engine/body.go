package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// errBodyRetired stops a read that outlives its attempt. net/http may keep
// reading a request body for a moment after RoundTrip has returned.
var errBodyRetired = errors.New("engine: request body used after its attempt ended")

// meteredBody is the request body of one upload attempt. It exposes only Read and
// Seek, so the HTTP stack cannot bypass metering through WriterTo or ReaderFrom.
//
// Between MeterQuantum flushes a Read costs one atomic add and one addition.
type meteredBody struct {
	ctx      context.Context
	section  *io.SectionReader
	length   int64
	activity *AttemptActivity
	limiter  *rateLimiter

	// transferred is the job's progress counter.
	transferred *atomic.Int64

	position int64 // Offset of the next Read within the section.
	pending  int64 // Bytes read but not yet published.
	started  bool

	// localErr is the first failure reading local data. The attempt reports it
	// instead of whatever the HTTP stack makes of the failed read.
	localErr atomic.Pointer[error]

	mu        sync.Mutex
	published int64 // Bytes this attempt has added to transferred.
	retired   bool
}

func (b *meteredBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		b.activity.SetPhase(AttemptSend)
	}
	n, err := b.section.Read(p)
	b.activity.Touch()
	b.position += int64(n)
	b.pending += int64(n)

	switch {
	case err == nil:
		if b.pending >= MeterQuantum {
			if flushErr := b.flush(); flushErr != nil {
				return n, flushErr
			}
		}
		return n, nil
	case errors.Is(err, io.EOF):
		if b.position != b.length {
			// The file shrank after the job was planned.
			return n, b.failLocal(fmt.Errorf("%w: source ended %d bytes early", ErrLocalIO, b.length-b.position))
		}
		if flushErr := b.flush(); flushErr != nil {
			return n, flushErr
		}
		b.activity.SetPhase(AttemptAwaitResponse)
		return n, io.EOF
	default:
		return n, b.failLocal(fmt.Errorf("%w: %w", ErrLocalIO, err))
	}
}

func (b *meteredBody) failLocal(err error) error {
	b.localErr.CompareAndSwap(nil, &err)
	return err
}

// flush publishes pending bytes and charges them to the bandwidth limiter. It
// also notices a cancelled attempt, because some stores only stop when Read fails.
func (b *meteredBody) flush() error {
	n := b.pending
	b.pending = 0

	b.mu.Lock()
	if b.retired {
		b.mu.Unlock()
		return errBodyRetired
	}
	b.published += n
	b.transferred.Add(n)
	b.mu.Unlock()

	if b.ctx.Err() != nil {
		return context.Cause(b.ctx)
	}
	if n == 0 || !b.limiter.limited.Load() {
		return nil
	}
	b.activity.Park()
	defer b.activity.Unpark()
	return b.limiter.WaitN(b.ctx, int(n))
}

// Seek repositions the body. A seek back to the start means the HTTP stack is
// sending again, so the bytes counted so far are rolled back.
func (b *meteredBody) Seek(offset int64, whence int) (int64, error) {
	position, err := b.section.Seek(offset, whence)
	if err != nil {
		return position, err
	}
	b.position = position
	if position == 0 {
		b.pending = 0
		b.started = false
		b.mu.Lock()
		if !b.retired {
			b.transferred.Add(-b.published)
			b.published = 0
		}
		b.mu.Unlock()
		b.activity.SetPhase(AttemptAwaitResponse)
	}
	return position, nil
}

// retire ends the body's right to change progress and returns the bytes it
// published. The scheduler calls it when the attempt's result arrives.
func (b *meteredBody) retire() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.retired = true
	return b.published
}

// localFailure returns the stored local read failure, or nil.
func (b *meteredBody) localFailure() error {
	if err := b.localErr.Load(); err != nil {
		return *err
	}
	return nil
}
