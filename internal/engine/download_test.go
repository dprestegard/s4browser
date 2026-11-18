package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/fsx"
	"s4browser/internal/journal"
)

const (
	dlKey  = "s4browser-test/run1/download.bin"
	dlPath = "downloads/movie.bin"
)

var dlRef = engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: dlKey}

// putObject creates a synthetic object to download.
func (h *harness) putObject(key string, size int64) {
	h.t.Helper()
	if err := h.server.PutSynthetic(fakes3.DefaultBucket, key, testSeed, size); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) download(path string) engine.JobID {
	h.t.Helper()
	id, err := h.engine.Submit(context.Background(), engine.JobSpec{Direction: engine.Download, Ref: dlRef, LocalPath: path})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// assertWrittenOnce proves every byte reached the synthetic sink exactly once,
// which also proves that no finished range was fetched again.
func (h *harness) assertWrittenOnce(path string, size int64) {
	h.t.Helper()
	total, distinct := h.sink.BytesWritten(path)
	if total != size || distinct != size {
		h.t.Fatalf("%s: %d bytes written, %d distinct, want %d of each", path, total, distinct, size)
	}
	if !h.sink.Committed(path) {
		h.t.Fatalf("%s was not committed", path)
	}
}

// getLog records every ranged GET that reaches the store.
type getLog struct {
	engine.ObjectStore
	mu       sync.Mutex
	requests []engine.GetRangeRequest
}

func (g *getLog) GetObjectRange(ctx context.Context, req engine.GetRangeRequest) (engine.GetRangeResult, error) {
	g.mu.Lock()
	g.requests = append(g.requests, req)
	g.mu.Unlock()
	return g.ObjectStore.GetObjectRange(ctx, req)
}

func (g *getLog) all() []engine.GetRangeRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]engine.GetRangeRequest(nil), g.requests...)
}

func withGetLog(log **getLog) func(engine.ObjectStore) engine.ObjectStore {
	return func(inner engine.ObjectStore) engine.ObjectStore {
		*log = &getLog{ObjectStore: inner}
		return *log
	}
}

func TestDownloadVerifiesEveryByteOnce(t *testing.T) {
	var log *getLog
	h := newHarness(t, options{wrap: withGetLog(&log)})
	h.putObject(dlKey, testSize)
	id := h.download(dlPath)
	job := h.waitState(id, engine.JobCompleted)
	if job.DoneBytes != testSize || job.TotalBytes != testSize || job.PartsDone != testParts || job.Retries != 0 {
		t.Fatalf("final snapshot %+v", job)
	}
	h.assertWrittenOnce(dlPath, testSize)
	h.assertClean()
	for _, req := range log.all() {
		if req.IfMatch == "" || req.Size != testSize {
			t.Fatalf("request without If-Match or size: %+v", req)
		}
	}
	if n := len(log.all()); n != testParts {
		t.Fatalf("%d GETs for %d parts", n, testParts)
	}
}

func TestSmallDownloadIsNotJournaled(t *testing.T) {
	var puts atomic.Int32
	h := newHarness(t, options{journal: func(inner journal.Store) journal.Store { return countingJournal{inner, &puts} }})
	h.putObject(dlKey, 10*mib)
	h.waitState(h.download(dlPath), engine.JobCompleted)
	h.assertWrittenOnce(dlPath, 10*mib)
	if puts.Load() != 0 {
		t.Fatalf("%d journal writes for a small download", puts.Load())
	}
}

type countingJournal struct {
	journal.Store
	puts *atomic.Int32
}

func (c countingJournal) Put(ctx context.Context, e journal.Entry) error {
	c.puts.Add(1)
	return c.Store.Put(ctx, e)
}

func TestZeroByteDownloadSendsNoGet(t *testing.T) {
	h := newHarness(t, options{})
	h.putObject(dlKey, 0)
	job := h.waitState(h.download(dlPath), engine.JobCompleted)
	if job.TotalBytes != 0 || job.PartsTotal != 0 {
		t.Fatalf("%+v", job)
	}
	if !h.sink.Committed(dlPath) {
		t.Fatal("the empty file was not committed")
	}
	if n := h.server.Attempts(fakes3.OpGet, fakes3.DefaultBucket, dlKey, 0); n != 0 {
		t.Fatalf("%d GETs for an empty object", n)
	}
}

// TestDownloadFaults checks each injected fault type against its designed
// reaction. A failing attempt keeps the bytes it wrote, so the retry asks only
// for the rest of the range.
func TestDownloadFaults(t *testing.T) {
	part3 := 2 * 5 * mib // Offset of part 3.
	cases := []struct {
		name    string
		fault   fakes3.Fault
		retries bool
		// resumes means the retry must start inside the part, not at its start.
		resumes bool
		timeout time.Duration
	}{
		{name: "InternalError", fault: fakes3.InternalError(), retries: true},
		{name: "ServiceUnavailable", fault: fakes3.ServiceUnavailable(), retries: true},
		{name: "SlowDown", fault: fakes3.SlowDown().WithRetryAfter(time.Millisecond), retries: true},
		{name: "ResetMidBody", fault: fakes3.ResetAt(2*mib + 17), retries: true, resumes: true},
		{name: "TruncatedBody", fault: fakes3.TruncateAt(3 * mib), retries: true, resumes: true},
		{name: "StallMidBody", fault: fakes3.StallAt(mib, 0), retries: true, resumes: true, timeout: time.Second},
		{name: "SlowResponse", fault: fakes3.SlowResponse(20 * time.Millisecond)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var log *getLog
			h := newHarness(t, options{wrap: withGetLog(&log), timeouts: func(to *engine.Timeouts) {
				if tc.timeout > 0 {
					to.Stall = tc.timeout
				}
			}})
			h.putObject(dlKey, testSize)
			at := part3
			h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, RangeStart: &at, Attempt: 0, Times: 1, Fault: tc.fault})
			job := h.waitState(h.download(dlPath), engine.JobCompleted)
			h.assertWrittenOnce(dlPath, testSize)
			h.assertClean()
			if tc.retries != (job.Retries > 0) {
				t.Fatalf("retries %d, want retries=%v", job.Retries, tc.retries)
			}
			if job.Retries > 1 {
				t.Fatalf("one fault cost %d retries", job.Retries)
			}
			var retried []engine.GetRangeRequest
			for _, req := range log.all() {
				if req.Offset >= part3 && req.Offset < part3+5*mib {
					retried = append(retried, req)
				}
			}
			want := 1
			if tc.retries {
				want = 2
			}
			if len(retried) != want {
				t.Fatalf("part 3 was requested %d times, want %d: %+v", len(retried), want, retried)
			}
			if tc.resumes {
				if retried[1].Offset <= part3 || retried[1].Offset+retried[1].Length != part3+5*mib {
					t.Fatalf("retry did not resume inside the part: %+v", retried[1])
				}
			}
		})
	}
}

func TestDownloadWaitsForCredentialsAndResumes(t *testing.T) {
	h := newHarness(t, options{})
	h.putObject(dlKey, testSize)
	h.prober.failures.Store(1 << 30)
	rule := h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Attempt: 3, Times: 1, Fault: fakes3.ExpiredToken()})
	id := h.download(dlPath)
	job := h.waitState(id, engine.JobCredentialWait)
	if job.Err != nil {
		t.Fatalf("the job recorded an error: %+v", job.Err)
	}
	h.waitFor(id, "probes", func(engine.JobSnapshot) bool { return h.prober.calls.Load() >= 2 })
	h.prober.failures.Store(0)
	h.engine.NotifyCredentialsChanged(dlRef.Profile)
	job = h.waitState(id, engine.JobCompleted)
	if job.Retries != 0 {
		t.Errorf("a credential failure was charged as a retry: %d", job.Retries)
	}
	if h.server.RuleFired(rule) != 1 {
		t.Fatal("the credential fault never fired")
	}
	h.assertWrittenOnce(dlPath, testSize)
	h.assertClean()
}

func TestRetryBudgetEndsDownload(t *testing.T) {
	h := newHarness(t, options{})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.InternalError()})
	job := h.waitState(h.download(dlPath), engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassRetryable {
		t.Fatalf("%+v", job.Err)
	}
	if !h.sink.Aborted(dlPath) || h.sink.Committed(dlPath) {
		t.Fatal("a failed download must abort its temp file and commit nothing")
	}
	h.assertClean()
}

func TestChangedETagFailsAndLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	h := newHarness(t, options{sink: engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Attempt: 4, Times: 1, Fault: fakes3.ChangeObject()})
	job := h.waitState(h.download(dest), engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassPermanent || !strings.Contains(job.Err.Message, "changed") {
		t.Fatalf("the failure must say the object changed: %+v", job.Err)
	}
	if job.Retries != 0 {
		t.Fatalf("a changed object was retried %d times", job.Retries)
	}
	assertEmptyDir(t, dir)
	h.assertClean()
}

func TestChangedSizeIsObjectChanged(t *testing.T) {
	// The ETag still matches, but the object is a different length.
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		return shrinkingHead{inner}
	}})
	h.putObject(dlKey, testSize)
	job := h.waitState(h.download(dlPath), engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassPermanent {
		t.Fatalf("%+v", job.Err)
	}
	if h.sink.Committed(dlPath) {
		t.Fatal("committed")
	}
}

// shrinkingHead reports a size that differs from the object's real size.
type shrinkingHead struct{ engine.ObjectStore }

func (s shrinkingHead) HeadObject(ctx context.Context, ref engine.ObjectRef) (engine.ObjectInfo, error) {
	info, err := s.ObjectStore.HeadObject(ctx, ref)
	info.Size -= mib
	return info, err
}

func TestCancelLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	h := newHarness(t, options{sink: engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.SlowBody(4 * mib)})
	id := h.download(dest)
	h.waitFor(id, "bytes", func(j engine.JobSnapshot) bool { return j.DoneBytes > 4*mib })
	if entries := h.journal.List(); len(entries) != 1 || entries[0].Kind != journal.KindDownloadTemp {
		t.Fatalf("journal while downloading: %+v", entries)
	}
	if err := h.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, engine.JobCancelled)
	assertEmptyDir(t, dir)
	h.assertClean()
}

func TestPauseAndResumeDownload(t *testing.T) {
	var log *getLog
	h := newHarness(t, options{wrap: withGetLog(&log)})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.SlowBody(8 * mib)})
	id := h.download(dlPath)
	h.waitFor(id, "bytes", func(j engine.JobSnapshot) bool { return j.DoneBytes > 8*mib })
	if err := h.engine.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	paused := h.waitState(id, engine.JobPaused)
	if paused.DoneBytes == 0 {
		t.Fatal("nothing was kept")
	}
	h.server.ClearRules()
	if err := h.engine.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, engine.JobCompleted)
	h.assertWrittenOnce(dlPath, testSize)
	h.assertClean()
}

func TestInsufficientSpaceFailsClearly(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	sink := engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))
	sink.FreeSpace = func(string) (int64, error) { return 10 * mib, nil }
	h := newHarness(t, options{sink: sink})
	h.putObject(dlKey, testSize)
	job := h.waitState(h.download(dest), engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassPermanent ||
		!strings.Contains(job.Err.Message, "needs") || !strings.Contains(job.Err.Message, "free") {
		t.Fatalf("unclear error: %+v", job.Err)
	}
	if !strings.Contains(job.Err.Message, "insufficient disk space") {
		t.Fatalf("the sentinel is missing: %q", job.Err.Message)
	}
	if job.DoneBytes != 0 {
		t.Fatalf("bytes were fetched: %d", job.DoneBytes)
	}
	assertEmptyDir(t, dir)
	h.assertClean() // The journal entry written before Prepare is deleted again.
}

func TestExistingDestinationIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	if err := os.WriteFile(dest, []byte("mine"), 0o666); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, options{sink: engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))})
	h.putObject(dlKey, testSize)
	job := h.waitState(h.download(dest), engine.JobFailed)
	if job.Err == nil || !strings.Contains(job.Err.Message, "already exists") {
		t.Fatalf("%+v", job.Err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "mine" {
		t.Fatalf("the existing file was changed: %q", data)
	}
	assertSingleFile(t, dir, "movie.bin")
	h.assertClean()
}

func TestDestinationAppearingBeforeCommitIsKept(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	h := newHarness(t, options{sink: engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.SlowBody(8 * mib)})
	id := h.download(dest)
	h.waitFor(id, "bytes", func(j engine.JobSnapshot) bool { return j.DoneBytes > 4*mib })
	if err := os.WriteFile(dest, []byte("mine"), 0o666); err != nil {
		t.Fatal(err)
	}
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || !strings.Contains(job.Err.Message, "already exists") {
		t.Fatalf("%+v", job.Err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "mine" {
		t.Fatalf("the file that appeared was replaced: %q", data)
	}
	assertSingleFile(t, dir, "movie.bin")
	h.assertClean()
}

// TestTempFileIsJournaledBeforeItExists pins the design's crash window: the
// journal entry exists before the file does, so a crash cannot leave an unknown file.
func TestTempFileIsJournaledBeforeItExists(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	var violations atomic.Int32
	var seen atomic.Pointer[journal.Entry]
	h := newHarness(t, options{
		sink: engine.NewFileSink(mustStrategy(t, fsx.StrategySparse)),
		journal: func(inner journal.Store) journal.Store {
			return putHook{inner, func(e journal.Entry) {
				seen.Store(&e)
				if _, err := os.Stat(e.TempPath); err == nil {
					violations.Add(1)
				}
			}}
		},
	})
	h.putObject(dlKey, testSize)
	id := h.download(dest)
	h.waitState(id, engine.JobCompleted)
	entry := seen.Load()
	if entry == nil || entry.Kind != journal.KindDownloadTemp || entry.Phase != journal.PhaseActive || !fsx.IsTempPath(entry.TempPath) ||
		filepath.Dir(entry.TempPath) != dir || entry.TempPath != fsx.TempPath(dest, string(id)) {
		t.Fatalf("journal entry %+v", entry)
	}
	if violations.Load() != 0 {
		t.Fatal("the temp file existed before its journal entry")
	}
	assertSingleFile(t, dir, "movie.bin")
	h.assertClean()
}

type putHook struct {
	journal.Store
	hook func(journal.Entry)
}

func (p putHook) Put(ctx context.Context, e journal.Entry) error {
	p.hook(e)
	return p.Store.Put(ctx, e)
}

func TestFailedJournalWriteFailsBeforeAnyFile(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, options{
		sink: engine.NewFileSink(mustStrategy(t, fsx.StrategySparse)),
		journal: func(inner journal.Store) journal.Store {
			return failingJournal{inner}
		},
	})
	h.putObject(dlKey, testSize)
	job := h.waitState(h.download(filepath.Join(dir, "movie.bin")), engine.JobFailed)
	if job.Err == nil || !strings.Contains(job.Err.Message, "journal") {
		t.Fatalf("%+v", job.Err)
	}
	assertEmptyDir(t, dir)
}

// TestDownloadFileStrategies downloads to real files with every strategy that can
// run here and compares the result with the synthetic pattern.
func TestDownloadFileStrategies(t *testing.T) {
	for _, name := range fsx.Names() {
		t.Run(name, func(t *testing.T) {
			strategy, err := fsx.NewStrategy(name, 12*mib)
			if errors.Is(err, fsx.ErrUnsupportedStrategy) {
				t.Skipf("not available here: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			dest := filepath.Join(dir, "movie.bin")
			h := newHarness(t, options{sink: engine.NewFileSink(strategy)})
			h.putObject(dlKey, testSize)
			job := h.waitState(h.download(dest), engine.JobCompleted)
			if job.DoneBytes != testSize {
				t.Fatalf("%+v", job)
			}
			assertPattern(t, dest, testSize)
			assertSingleFile(t, dir, "movie.bin")
			h.assertClean()
		})
	}
}

func mustStrategy(t *testing.T, name string) fsx.WriteStrategy {
	t.Helper()
	s, err := fsx.NewStrategy(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("%s must be empty, holds %v", dir, names)
	}
}

func assertSingleFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("%s must hold only %s, holds %v", dir, name, names)
	}
}

// assertPattern reads a finished file and compares every byte with the pattern.
func assertPattern(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != size {
		t.Fatalf("size %v, want %d (%v)", info, size, err)
	}
	buf := make([]byte, mib)
	scratch := make([]byte, mib)
	for off := int64(0); off < size; off += int64(len(buf)) {
		n, err := f.ReadAt(buf, off)
		if n == 0 && err != nil {
			t.Fatal(err)
		}
		if at, ok := (fakes3.Pattern{Seed: testSeed}).Equal(buf[:n], off, scratch); !ok {
			t.Fatalf("byte %d differs from the source", at)
		}
	}
}

// ---- Lead gate ----

// TestLeadGateCompletesWithoutDeadlock forces the case preemption exists for: the
// only slot of the file is held by a parked attempt while the lowest part waits in
// backoff.
func TestLeadGateCompletesWithoutDeadlock(t *testing.T) {
	sink := fakes3.NewSink(func(string) uint64 { return testSeed })
	sink.MaxLead = 7*mib + mib/2 // Part 2 starts inside the gate and parks at 8 MiB.
	h := newHarness(t, options{
		sink: sink,
		settings: func(s *engine.Settings) {
			s.MaxActiveFiles, s.MaxPartsPerFile, s.MaxActiveParts = 1, 1, 1
		},
		retry: func(r *engine.RetryPolicy) { r.BaseDelay, r.MaxDelay = 300*time.Millisecond, 300*time.Millisecond },
	})
	h.putObject(dlKey, 40*mib)
	zero := int64(0)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, RangeStart: &zero, Attempt: 1, Fault: fakes3.InternalError()})
	job := h.waitState(h.download(dlPath), engine.JobCompleted)
	if job.DoneBytes != 40*mib {
		t.Fatalf("%+v", job)
	}
	total, distinct := sink.BytesWritten(dlPath)
	if total != 40*mib || distinct != 40*mib {
		t.Fatalf("%d bytes written, %d distinct", total, distinct)
	}
	stats := h.engine.(engine.StatsProvider).DownloadStats()
	if stats.Parks == 0 || stats.Preemptions == 0 {
		t.Fatalf("the gate never parked or preempted: %+v", stats)
	}
	h.assertClean()
}

// TestLeadGateKeepsWritesInsideTheLead records the furthest write ahead of the
// lowest unfinished byte.
func TestLeadGateKeepsWritesInsideTheLead(t *testing.T) {
	const lead = 12 * mib
	rec := &leadRecorder{lead: lead, partSize: testPartSize}
	h := newHarness(t, options{sink: rec})
	h.putObject(dlKey, testSize)
	h.waitState(h.download(dlPath), engine.JobCompleted)
	if rec.worst.Load() > lead {
		t.Fatalf("a write started %d bytes ahead of the lowest finished part, lead %d", rec.worst.Load(), lead)
	}
	if rec.writes.Load() == 0 {
		t.Fatal("no writes")
	}
}

// leadRecorder is a sink that measures how far ahead of the file's contiguous
// prefix a write starts. A write may only start inside the lead of the lowest
// unfinished part, and parts finish in order of their last byte.
type leadRecorder struct {
	lead, partSize int64
	worst, writes  atomic.Int64
	mu             sync.Mutex
	got            map[int64]int64 // bytes written per part
}

func (r *leadRecorder) Prepare(_ context.Context, spec engine.SinkSpec) (engine.SinkWriter, error) {
	r.got = make(map[int64]int64)
	return &leadWriter{r: r, size: spec.Size}, nil
}

type leadWriter struct {
	r    *leadRecorder
	size int64
}

func (w *leadWriter) WriteAt(p []byte, off int64) (int, error) {
	r := w.r
	r.mu.Lock()
	defer r.mu.Unlock()
	part := off / r.partSize
	r.got[part] += int64(len(p))
	// The frontier is the start of the lowest part not yet complete.
	frontier := int64(0)
	for {
		length := min(r.partSize, w.size-frontier)
		if frontier >= w.size || r.got[frontier/r.partSize] < length {
			break
		}
		frontier += r.partSize
	}
	// The scheduler's frontier moves only when the result is processed, so allow one buffer of lag.
	if ahead := off - frontier - engine.DownloadBufferSize; ahead > r.worst.Load() {
		r.worst.Store(ahead)
	}
	r.writes.Add(1)
	return len(p), nil
}
func (w *leadWriter) MaxLead() int64               { return w.r.lead }
func (w *leadWriter) Commit(context.Context) error { return nil }
func (w *leadWriter) Abort() error                 { return nil }

// ---- Bandwidth and progress ----

func TestDownloadHonoursBandwidthLimit(t *testing.T) {
	h := newHarness(t, options{settings: func(s *engine.Settings) { s.DownloadBytesPerSec = 40 * mib }})
	h.putObject(dlKey, 24*mib)
	start := time.Now()
	h.waitState(h.download(dlPath), engine.JobCompleted)
	// The burst is rate/10, so 24 MiB needs about (24-4)/40 s.
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("24 MiB at a 40 MiB/s limit took %v", elapsed)
	}
	h.assertWrittenOnce(dlPath, 24*mib)
}

func TestDownloadProgressIsBatched(t *testing.T) {
	h := newHarness(t, options{})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.SlowBody(16 * mib)})
	start := time.Now()
	id := h.download(dlPath)
	h.waitState(id, engine.JobCompleted)
	elapsed := time.Since(start)
	h.close()
	var last engine.JobSnapshot
	batches := 0
	for _, b := range h.obs.all() {
		for _, j := range b.Jobs {
			if j.ID == id {
				last = j
				batches++
			}
		}
	}
	if last.State != engine.JobCompleted || last.DoneBytes != testSize {
		t.Fatalf("last reported snapshot %+v", last)
	}
	if limit := int(elapsed/engine.ProgressInterval) + 4; batches > limit {
		t.Fatalf("%d progress batches in %v, expected at most %d", batches, elapsed, limit)
	}
}

// TestLeadSmallerThanAPartCannotStopTheLowestPart: the part that has to finish
// first writes beyond frontier+lead, so the gate must let it through.
func TestLeadSmallerThanAPartCannotStopTheLowestPart(t *testing.T) {
	sink := fakes3.NewSink(func(string) uint64 { return testSeed })
	sink.MaxLead = mib
	h := newHarness(t, options{sink: sink})
	h.putObject(dlKey, 20*mib)
	job := h.waitState(h.download(dlPath), engine.JobCompleted)
	if job.DoneBytes != 20*mib {
		t.Fatalf("%+v", job)
	}
	if total, distinct := sink.BytesWritten(dlPath); total != 20*mib || distinct != 20*mib {
		t.Fatalf("%d written, %d distinct", total, distinct)
	}
}
