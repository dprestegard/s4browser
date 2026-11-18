package engine_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
)

const (
	mib      = int64(1) << 20
	gib      = int64(1) << 30
	testSeed = uint64(0x5eed)
	testPath = "source.bin"
	testKey  = "s4browser-test/run1/object.bin"
	// testSize with testPartSize gives 16 parts, quick enough for every fault case.
	testSize     = 80 * mib
	testPartSize = 5 * mib
	testParts    = 16
)

var testRef = engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: testKey}

// scriptedProber fails the first failures probes.
type scriptedProber struct {
	failures atomic.Int32
	calls    atomic.Int32
}

func (p *scriptedProber) Probe(context.Context, string) error {
	p.calls.Add(1)
	if p.failures.Add(-1) >= 0 {
		return errors.New("credentials still unavailable")
	}
	return nil
}

type recordingObserver struct {
	mu      sync.Mutex
	batches []engine.ProgressBatch
}

func (o *recordingObserver) OnProgress(batch engine.ProgressBatch) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.batches = append(o.batches, batch)
}

func (o *recordingObserver) all() []engine.ProgressBatch {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]engine.ProgressBatch(nil), o.batches...)
}

type harness struct {
	t       *testing.T
	server  *fakes3.Server
	source  *fakes3.Source
	sink    *fakes3.Sink
	journal *journal.FileStore
	dir     string
	prober  *scriptedProber
	obs     *recordingObserver
	engine  engine.Engine
	closed  bool
}

type options struct {
	settings func(*engine.Settings)
	retry    func(*engine.RetryPolicy)
	timeouts func(*engine.Timeouts)
	// wrap decorates the store under test.
	wrap func(engine.ObjectStore) engine.ObjectStore
	// journal replaces the file journal given to the engine.
	journal func(journal.Store) journal.Store
	// sink replaces the synthetic sink given to the engine.
	sink engine.Sink
	// discardProgress drops batches, so that memory tests do not measure them.
	discardProgress bool
}

type discardObserver struct{}

func (discardObserver) OnProgress(engine.ProgressBatch) {}

func fastRetry() engine.RetryPolicy {
	return engine.RetryPolicy{
		BaseDelay: time.Millisecond, ThrottleBaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
		MaxPartAttempts: 3, MaxControlAttempts: 3, MaxAbortAttempts: 3, OutageTolerance: 0,
	}
}

// newHarness starts an engine on the in-process fake. Its cleanup closes
// everything and fails the test if any goroutine outlives the engine.
func newHarness(t *testing.T, opts options) *harness {
	t.Helper()
	baseline := runtime.NumGoroutine()

	guard, err := engine.NewTestPrefixGuard("run1")
	if err != nil {
		t.Fatal(err)
	}
	server, err := fakes3.New(fakes3.Options{Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := journal.OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, server: server, source: fakes3.NewSource(), journal: store, dir: dir,
		prober: new(scriptedProber), obs: new(recordingObserver),
	}
	h.source.Set(testPath, fakes3.SyntheticFile{Size: testSize, Seed: testSeed})
	h.sink = fakes3.NewSink(func(string) uint64 { return testSeed })
	var sink engine.Sink = h.sink
	if opts.sink != nil {
		sink = opts.sink
	}

	settings := engine.DefaultSettings()
	settings.PartSize = testPartSize
	if opts.settings != nil {
		opts.settings(&settings)
	}
	retry := fastRetry()
	if opts.retry != nil {
		opts.retry(&retry)
	}
	timeouts := engine.DefaultTimeouts()
	timeouts.ProbeMin, timeouts.ProbeMax = 5*time.Millisecond, 20*time.Millisecond
	if opts.timeouts != nil {
		opts.timeouts(&timeouts)
	}
	objectStore := fakes3.GuardStore(server.Store(), guard)
	if opts.wrap != nil {
		objectStore = opts.wrap(objectStore)
	}
	var journalStore journal.Store = store
	if opts.journal != nil {
		journalStore = opts.journal(store)
	}
	var observer engine.Observer = h.obs
	if opts.discardProgress {
		observer = discardObserver{}
	}
	h.engine, err = engine.New(engine.Config{
		Store: objectStore, Source: h.source, Sink: sink,
		Journal: journalStore, Prober: h.prober, Clock: engine.RealClock{}, Observer: observer, Guard: guard,
		Jitter: func() float64 { return 0.5 }, Settings: settings, Retry: retry, Timeouts: timeouts,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.close()
		server.Close()
		store.Close()
		assertNoLeak(t, baseline)
	})
	return h
}

func (h *harness) close() {
	h.t.Helper()
	if h.closed {
		return
	}
	h.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.engine.Close(ctx); err != nil {
		h.t.Errorf("Close: %v", err)
	}
}

func assertNoLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) submit() engine.JobID {
	h.t.Helper()
	return h.submitAs(testPath, testRef)
}

func (h *harness) submitAs(path string, ref engine.ObjectRef) engine.JobID {
	h.t.Helper()
	id, err := h.engine.Submit(context.Background(), engine.JobSpec{Direction: engine.Upload, Ref: ref, LocalPath: path})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) snapshot(id engine.JobID) engine.JobSnapshot {
	h.t.Helper()
	all, err := h.engine.Snapshot(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, job := range all {
		if job.ID == id {
			return job
		}
	}
	h.t.Fatalf("job %s not found", id)
	return engine.JobSnapshot{}
}

func (h *harness) waitFor(id engine.JobID, what string, ok func(engine.JobSnapshot) bool) engine.JobSnapshot {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		job := h.snapshot(id)
		if ok(job) {
			return job
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s: %+v", what, job)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) waitState(id engine.JobID, state engine.JobState) engine.JobSnapshot {
	h.t.Helper()
	return h.waitFor(id, state.String(), func(job engine.JobSnapshot) bool {
		if job.State.IsTerminal() && job.State != state {
			h.t.Fatalf("job ended %s, want %s: %+v", job.State, state, job.Err)
		}
		return job.State == state
	})
}

// assertClean proves no multipart upload and no journal entry remain.
func (h *harness) assertClean() {
	h.t.Helper()
	if uploads := h.server.ActiveUploads(fakes3.DefaultBucket); len(uploads) != 0 {
		h.t.Errorf("multipart uploads remain: %+v", uploads)
	}
	if entries := h.journal.List(); len(entries) != 0 {
		h.t.Errorf("journal entries remain: %+v", entries)
	}
}

// assertVerified proves the stored object matches the synthetic source.
func (h *harness) assertVerified(key string, seed uint64, size int64) {
	h.t.Helper()
	object, ok := h.server.Object(fakes3.DefaultBucket, key)
	if !ok {
		h.t.Fatalf("object %s does not exist", key)
	}
	if object.Size != size {
		h.t.Fatalf("object is %d bytes, want %d", object.Size, size)
	}
	if err := h.server.VerifyObject(fakes3.DefaultBucket, key, seed, 1); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) attempts(part int32) int {
	return h.server.Attempts(fakes3.OpUploadPart, fakes3.DefaultBucket, testKey, part)
}

// gateStore holds part uploads at a gate and records their concurrency.
type gateStore struct {
	engine.ObjectStore
	gate chan struct{} // Each receive admits one part. Closed admits all.

	mu      sync.Mutex
	current int
	peak    int
	started int
}

func newGateStore(inner engine.ObjectStore) *gateStore {
	return &gateStore{ObjectStore: inner, gate: make(chan struct{})}
}

func (g *gateStore) UploadPart(ctx context.Context, req engine.UploadPartRequest) (engine.PutResult, error) {
	g.mu.Lock()
	g.current++
	g.started++
	g.peak = max(g.peak, g.current)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.current--
		g.mu.Unlock()
	}()
	select {
	case <-g.gate:
	case <-ctx.Done():
		return engine.PutResult{}, context.Cause(ctx)
	}
	return g.ObjectStore.UploadPart(ctx, req)
}

func (g *gateStore) stats() (current, peak, started int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current, g.peak, g.started
}

func (g *gateStore) resetPeak() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.peak = g.current
}

func (g *gateStore) waitCurrent(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		current, _, _ := g.stats()
		if current == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d parts in flight, want %d", current, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// release admits n parts.
func (g *gateStore) release(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case g.gate <- struct{}{}:
		case <-time.After(30 * time.Second):
			t.Fatal("no part arrived at the gate")
		}
	}
}

// completeHook replaces CompleteMultipartUpload's result or blocks it.
type completeHook struct {
	engine.ObjectStore
	hook func(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error)
}

func (c completeHook) CompleteMultipartUpload(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
	return c.hook(ctx, req)
}
