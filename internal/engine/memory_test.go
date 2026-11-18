//go:build !race

// Memory assertions are excluded under the race detector, which multiplies heap
// use (docs/engine-design.md section 4).

package engine_test

import (
	"context"
	"fmt"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
)

const raceEnabled = false

func heapObjects() int64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return int64(sample[0].Value.Uint64())
}

// heapSampler records the peak heap every 100 ms, as the design prescribes.
type heapSampler struct {
	baseline int64
	peak     atomic.Int64
	stop     chan struct{}
	wg       sync.WaitGroup
}

func startHeapSampler() *heapSampler {
	runtime.GC()
	s := &heapSampler{baseline: heapObjects(), stop: make(chan struct{})}
	s.wg.Go(func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if v := heapObjects(); v > s.peak.Load() {
				s.peak.Store(v)
			}
			select {
			case <-ticker.C:
			case <-s.stop:
				return
			}
		}
	})
	return s
}

// finish stops sampling and returns the peak growth over the baseline.
func (s *heapSampler) finish() int64 {
	close(s.stop)
	s.wg.Wait()
	return s.peak.Load() - s.baseline
}

// startMemoryCheck asserts the design's bound when the returned function runs.
func startMemoryCheck(t *testing.T, settings engine.Settings, jobs int) func() {
	t.Helper()
	sampler := startHeapSampler()
	return func() {
		t.Helper()
		growth := sampler.finish()
		limit := engine.MemoryAssertGCFactor * engine.MemoryBound(settings, jobs)
		t.Logf("peak heap growth %.1f MiB, limit %.1f MiB", float64(growth)/float64(mib), float64(limit)/float64(mib))
		if growth > limit {
			t.Fatalf("peak heap grew by %d bytes, limit %d", growth, limit)
		}
	}
}

// TestPeakMemoryDoesNotDependOnPartSize uploads the same object with 64 MiB and
// 1 GiB parts. Bodies are streamed, so the peaks must be nearly equal.
func TestPeakMemoryDoesNotDependOnPartSize(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 256 GiB through the CRC loop")
	}
	const size = 128 * gib
	run := func(partSize int64) int64 {
		var growth int64
		t.Run(fmt.Sprintf("%d MiB parts", partSize/mib), func(t *testing.T) {
			h := newHarness(t, options{discardProgress: true, settings: func(s *engine.Settings) { s.PartSize = partSize }})
			h.source.Set("big", fakes3.SyntheticFile{Size: size, Seed: testSeed})
			settings := engine.DefaultSettings()
			sampler := startHeapSampler()
			id := h.submitAs("big", testRef)
			h.waitState(id, engine.JobCompleted)
			growth = sampler.finish()
			limit := engine.MemoryAssertGCFactor * engine.MemoryBound(settings, 1)
			t.Logf("peak heap growth %.1f MiB, limit %.1f MiB", float64(growth)/float64(mib), float64(limit)/float64(mib))
			if growth > limit {
				t.Fatalf("peak heap grew by %d bytes, limit %d", growth, limit)
			}
		})
		return growth
	}
	small, large := run(64*mib), run(1*gib)
	if diff := max(small-large, large-small); diff >= 16*mib {
		t.Fatalf("peaks differ by %.1f MiB: %.1f MiB with 64 MiB parts, %.1f MiB with 1 GiB parts",
			float64(diff)/float64(mib), float64(small)/float64(mib), float64(large)/float64(mib))
	}
}

// TestQueuedJobRecordFitsBudget measures MemPerJobRecord.
func TestQueuedJobRecordFitsBudget(t *testing.T) {
	const jobs = 5000
	var gate *gateStore
	h := newHarness(t, options{discardProgress: true, wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}, settings: func(s *engine.Settings) { s.MaxActiveFiles = 1 }})
	h.submit()
	gate.waitCurrent(t, engine.DefaultPartsPerFile)

	runtime.GC()
	before := heapObjects()
	for i := range jobs {
		ref := testRef
		ref.Key = fmt.Sprintf("s4browser-test/run1/queued/%06d.bin", i)
		h.submitAs(testPath, ref)
	}
	time.Sleep(300 * time.Millisecond) // Let a progress batch pass.
	runtime.GC()
	perJob := (heapObjects() - before) / jobs
	t.Logf("measured %d bytes per queued job, budget %d", perJob, engine.MemPerJobRecord)
	if perJob > engine.MemPerJobRecord {
		t.Fatalf("a queued job uses %d bytes, budget %d", perJob, engine.MemPerJobRecord)
	}
	if err := h.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPartTableFitsBudget measures MemPerPartEntry on a job holding 10,000 parts.
func TestPartTableFitsBudget(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{discardProgress: true, wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		close(gate.gate)
		return partsOnly{gate}
	}})
	const parts = engine.MaxPartCount
	h.source.Set("big", fakes3.SyntheticFile{Size: parts * testPartSize, Seed: testSeed})

	runtime.GC()
	before := heapObjects()
	id := h.submitAs("big", testRef)
	h.waitFor(id, "every part", func(job engine.JobSnapshot) bool { return job.PartsDone == parts })
	runtime.GC()
	perPart := (heapObjects() - before) / parts
	t.Logf("measured %d bytes per finished part, budget %d", perPart, engine.MemPerPartEntry)
	if perPart > engine.MemPerPartEntry {
		t.Fatalf("a part uses %d bytes, budget %d", perPart, engine.MemPerPartEntry)
	}
}

// partsOnly answers parts without reading them or storing anything, so only the
// engine's own table is measured. Complete never returns until the engine closes.
type partsOnly struct{ engine.ObjectStore }

func (partsOnly) UploadPart(_ context.Context, req engine.UploadPartRequest) (engine.PutResult, error) {
	return engine.PutResult{
		ETag:          fmt.Sprintf(`"%032x"`, req.PartNumber),
		ChecksumCRC32: "AAAAAA==",
	}, nil
}

func (partsOnly) CompleteMultipartUpload(ctx context.Context, _ engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
	<-ctx.Done()
	return engine.CompleteUploadResult{}, context.Cause(ctx)
}
