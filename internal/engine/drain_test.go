package engine_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
)

// socketStore models an upload connection on a slow link. The first buffer
// bytes of a body are taken at once, as a socket send buffer takes them. The
// rest is taken at rate. Once the body has been read, the bytes still in the
// "buffer" take buffered/rate to leave, and only then does the server answer.
// This is what made Timeouts.Response too short on slow links (stage 0.7).
type socketStore struct {
	engine.ObjectStore
	buffer int64
	rate   float64 // bytes per second
}

func (s socketStore) send(ctx context.Context, body io.Reader, length int64) ([]byte, error) {
	data := make([]byte, 0, length)
	chunk := make([]byte, 16<<10)
	for {
		n, err := body.Read(chunk)
		data = append(data, chunk[:n]...)
		if over := int64(len(data)) - s.buffer; over > 0 && n > 0 {
			if err := sleepCtx(ctx, time.Duration(float64(min(int64(n), over))/s.rate*float64(time.Second))); err != nil {
				return nil, err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	drain := time.Duration(float64(min(length, s.buffer)) / s.rate * float64(time.Second))
	return data, sleepCtx(ctx, drain)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s socketStore) PutObject(ctx context.Context, req engine.PutObjectRequest) (engine.PutResult, error) {
	data, err := s.send(ctx, req.Body, req.ContentLength)
	if err != nil {
		return engine.PutResult{}, err
	}
	req.Body = bytes.NewReader(data)
	return s.ObjectStore.PutObject(ctx, req)
}

const (
	slowBuffer = 384 << 10
	slowRate   = 128 << 10
)

func slowLinkHarness(t *testing.T) *harness {
	return newHarness(t, options{
		sendBuffer: slowBuffer,
		wrap: func(inner engine.ObjectStore) engine.ObjectStore {
			return socketStore{ObjectStore: inner, buffer: slowBuffer, rate: slowRate}
		},
		timeouts: func(t *engine.Timeouts) { t.Response = time.Second },
		retry:    func(r *engine.RetryPolicy) { r.MaxPartAttempts = 10 },
	})
}

func (h *harness) submitSynthetic(name string, size int64) engine.JobID {
	h.t.Helper()
	h.source.Set(name, fakes3.SyntheticFile{Size: size, Seed: testSeed})
	return h.submitAs(name, engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "s4browser-test/run1/" + name})
}

// TestSlowLinkIsNotAStall: a body larger than two send buffers measures the
// link itself, and a smaller one uses what earlier attempts measured. Neither
// may be retried, although each waits longer than Timeouts.Response for the
// buffer to drain.
func TestSlowLinkIsNotAStall(t *testing.T) {
	h := slowLinkHarness(t)

	// 640 KiB beyond the buffer at 128 KiB/s takes 5 s, then 3 s to drain.
	big := h.submitSynthetic("big.bin", 1<<20)
	if job := h.waitState(big, engine.JobCompleted); job.Retries != 0 {
		t.Fatalf("the large body was retried %d times", job.Retries)
	}
	// Fits in the buffer at once, then 2 s to drain.
	small := h.submitSynthetic("small.bin", 256<<10)
	if job := h.waitState(small, engine.JobCompleted); job.Retries != 0 {
		t.Fatalf("the small body was retried %d times", job.Retries)
	}
	h.assertVerified("s4browser-test/run1/big.bin", testSeed, 1<<20)
	h.assertVerified("s4browser-test/run1/small.bin", testSeed, 256<<10)
}

// TestSlowLinkLearnsFromStalls: with nothing measured yet, a body that fits in
// the buffer gets no allowance and stalls. Each stall halves the rate estimate,
// so the allowance grows until the upload completes.
func TestSlowLinkLearnsFromStalls(t *testing.T) {
	h := newHarness(t, options{
		sendBuffer: slowBuffer,
		wrap: func(inner engine.ObjectStore) engine.ObjectStore {
			return socketStore{ObjectStore: inner, buffer: slowBuffer, rate: 32 << 10}
		},
		timeouts: func(t *engine.Timeouts) { t.Response = time.Second },
		retry:    func(r *engine.RetryPolicy) { r.MaxPartAttempts = 10 },
	})
	id := h.submitSynthetic("small.bin", 256<<10) // 8 s to drain. Halving covers it within 3 retries; growing by one Response per stall needs about 5.
	job := h.waitState(id, engine.JobCompleted)
	if job.Retries < 1 || job.Retries > 3 {
		t.Fatalf("retries %d, want 1 to 3", job.Retries)
	}
	h.assertVerified("s4browser-test/run1/small.bin", testSeed, 256<<10)
}

func TestNegativeSendBufferIsRejected(t *testing.T) {
	server, err := fakes3.New(fakes3.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	_, err = engine.New(engine.Config{
		Store: server.Store(), Source: fakes3.NewSource(), Sink: fakes3.NewSink(func(string) uint64 { return 0 }),
		Journal: discardJournal{}, Prober: new(scriptedProber), Clock: engine.RealClock{},
		Jitter: func() float64 { return 0 }, Settings: engine.DefaultSettings(), Retry: engine.DefaultRetryPolicy(),
		Timeouts: engine.DefaultTimeouts(), SendBuffer: -1,
	})
	if !errors.Is(err, engine.ErrInvalidConfig) {
		t.Fatalf("got %v", err)
	}
}

type discardJournal struct{ journal.Store }
