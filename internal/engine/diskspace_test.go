package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/fsx"
)

// These tests cover findings F1 and F3 of the stage 4.1 engine review (docs/review-4.1.md, removed;
// see git history).

// fullDrive is a write strategy whose files share a byte budget. Once it is
// spent, every write reports a full drive until refill.
type fullDrive struct {
	fsx.WriteStrategy
	mu     sync.Mutex
	budget int64 // < 0 means unlimited.
	full   atomic.Int32
}

func (d *fullDrive) Create(path string, size int64) (fsx.WriteTarget, error) {
	t, err := d.WriteStrategy.Create(path, size)
	if err != nil {
		return nil, err
	}
	return &fullTarget{WriteTarget: t, drive: d}, nil
}

func (d *fullDrive) take(n int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.budget < 0 {
		return n
	}
	allowed := int(min(int64(n), d.budget))
	d.budget -= int64(allowed)
	return allowed
}

func (d *fullDrive) refill() {
	d.mu.Lock()
	d.budget = -1
	d.mu.Unlock()
}

type fullTarget struct {
	fsx.WriteTarget
	drive *fullDrive
}

func (t *fullTarget) WriteAt(p []byte, off int64) (int, error) {
	allowed := t.drive.take(len(p))
	n, err := t.WriteTarget.WriteAt(p[:allowed], off)
	if err == nil && n < len(p) {
		t.drive.full.Add(1)
		err = &os.PathError{Op: "write", Path: "temp", Err: fsx.ErrDiskFull}
	}
	return n, err
}

func (t *fullTarget) Finish() error {
	if f, ok := t.WriteTarget.(fsx.Finisher); ok {
		return f.Finish()
	}
	return nil
}

// TestFullDrivePausesAndKeepsTheDownload is F1 part 2: a full drive pauses the
// job with the temp file, its journal entry and the written bytes kept, and
// Resume finishes it without fetching those bytes again.
func TestFullDrivePausesAndKeepsTheDownload(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	drive := &fullDrive{WriteStrategy: mustStrategy(t, fsx.StrategySparse), budget: 20 * mib}
	var log *getLog
	h := newHarness(t, options{sink: engine.NewFileSink(drive), wrap: withGetLog(&log)})
	h.putObject(dlKey, testSize)
	id := h.download(dest)

	paused := h.waitState(id, engine.JobPaused)
	if paused.Err != nil || paused.PauseReason != engine.DiskFullNotice || paused.Warning != "" {
		t.Fatalf("paused without the reason: %+v, err %+v", paused, paused.Err)
	}
	if paused.DoneBytes < 20*mib {
		t.Fatalf("only %d bytes were kept", paused.DoneBytes)
	}
	if drive.full.Load() == 0 {
		t.Fatal("the drive never reported full")
	}
	if _, err := os.Stat(fsx.TempPath(dest, string(id))); err != nil {
		t.Fatalf("the temp file is gone: %v", err)
	}
	if entries := h.journal.List(); len(entries) != 1 {
		t.Fatalf("journal while paused: %+v", entries)
	}
	before := fetched(log.all())

	drive.refill()
	if err := h.engine.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	done := h.waitState(id, engine.JobCompleted)
	if done.Warning != "" || done.PauseReason != "" || done.Retries != 0 {
		t.Fatalf("final snapshot %+v", done)
	}
	if after := fetched(log.all()) - before; after > testSize-20*mib {
		t.Fatalf("resume fetched %d bytes, at most %d were missing", after, testSize-20*mib)
	}
	assertPattern(t, dest, testSize)
	assertSingleFile(t, dir, "movie.bin")
	h.assertClean()
}

// heldDrive holds the write that finishes one byte range until release closes,
// and closes reached when that write starts.
type heldDrive struct {
	fsx.WriteStrategy
	end     int64 // the write ending here is held
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (d *heldDrive) Create(path string, size int64) (fsx.WriteTarget, error) {
	t, err := d.WriteStrategy.Create(path, size)
	if err != nil {
		return nil, err
	}
	return &heldTarget{WriteTarget: t, drive: d}, nil
}

type heldTarget struct {
	fsx.WriteTarget
	drive *heldDrive
}

func (t *heldTarget) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) == t.drive.end {
		t.drive.once.Do(func() { close(t.drive.reached) })
		<-t.drive.release
	}
	return t.WriteTarget.WriteAt(p, off)
}

func (t *heldTarget) Finish() error {
	if f, ok := t.WriteTarget.(fsx.Finisher); ok {
		return f.Finish()
	}
	return nil
}

// TestPauseAfterAPartsLastWriteKeepsThePartDone: an attempt that wrote every
// byte of its part and then saw the job's cancel (here a pause; in the field a
// full drive on another part) has finished the part. Recording it as partial
// left a zero-length range to fetch, and the resume failed the job with
// "invalid size". It flaked TestFullDrivePausesAndKeepsTheDownload on CI.
func TestPauseAfterAPartsLastWriteKeepsThePartDone(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	part3End := int64(3 * testPartSize)
	drive := &heldDrive{WriteStrategy: mustStrategy(t, fsx.StrategySparse), end: part3End,
		reached: make(chan struct{}), release: make(chan struct{})}
	var log *getLog
	h := newHarness(t, options{sink: engine.NewFileSink(drive), wrap: withGetLog(&log)})
	h.putObject(dlKey, testSize)
	id := h.download(dest)

	<-drive.reached
	if err := h.engine.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, engine.JobPausing) // the attempt's context is cancelled now
	close(drive.release)
	h.waitState(id, engine.JobPaused)

	if err := h.engine.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, engine.JobCompleted)
	for _, req := range log.all() {
		if req.Length <= 0 {
			t.Fatalf("a request for no bytes: %+v", req)
		}
		if req.Offset >= 2*testPartSize && req.Offset < part3End && req.Offset != 2*testPartSize {
			t.Fatalf("part 3 was fetched again after it was written: %+v", req)
		}
	}
	assertPattern(t, dest, testSize)
	h.assertClean()
}

// TestFullDriveThenCancelRemovesTheTempFile proves a job paused for a full
// drive is cleaned up like any paused download.
func TestFullDriveThenCancelRemovesTheTempFile(t *testing.T) {
	dir := t.TempDir()
	drive := &fullDrive{WriteStrategy: mustStrategy(t, fsx.StrategySparse), budget: 10 * mib}
	h := newHarness(t, options{sink: engine.NewFileSink(drive)})
	h.putObject(dlKey, testSize)
	id := h.download(filepath.Join(dir, "movie.bin"))
	h.waitState(id, engine.JobPaused)
	if err := h.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, engine.JobCancelled)
	assertEmptyDir(t, dir)
	h.assertClean()
}

func fetched(requests []engine.GetRangeRequest) int64 {
	var total int64
	for _, r := range requests {
		total += r.Length
	}
	return total
}

func fixedSpace(sink *engine.FileSink, free int64) {
	sink.FreeSpace = func(string) (int64, error) { return free, nil }
	sink.Volume = func(string) (string, error) { return "v", nil }
}

// TestSinkReservesSpaceAcrossDownloads is F1 part 1: two downloads that each
// fit alone are not both admitted to one volume, and the reservation is
// returned when a temp file is committed or removed.
func TestSinkReservesSpaceAcrossDownloads(t *testing.T) {
	dir := t.TempDir()
	sink := engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))
	fixedSpace(sink, 200*mib)
	spec := func(id, name string) engine.SinkSpec {
		return engine.SinkSpec{JobID: engine.JobID(id), DestPath: filepath.Join(dir, name), Size: 100 * mib}
	}
	ctx := context.Background()

	a, err := sink.Prepare(ctx, spec("aaaaaaaaaaaaaaaa", "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Reserved("v"); got != 100*mib {
		t.Fatalf("reserved %d", got)
	}
	_, err = sink.Prepare(ctx, spec("bbbbbbbbbbbbbbbb", "b.bin"))
	if !errors.Is(err, engine.ErrInsufficientSpace) || !strings.Contains(err.Error(), "other downloads") {
		t.Fatalf("second download admitted: %v", err)
	}

	// Bytes written are out of the free space already, so they leave the reservation.
	buf := make([]byte, mib)
	for off := int64(0); off < 40*mib; off += mib {
		if _, err := a.WriteAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if got := sink.Reserved("v"); got != 60*mib {
		t.Fatalf("reserved %d after 40 MiB written", got)
	}

	if err := a.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := sink.Reserved("v"); got != 0 {
		t.Fatalf("reserved %d after abort", got)
	}
	b, err := sink.Prepare(ctx, spec("bbbbbbbbbbbbbbbb", "b.bin"))
	if err != nil {
		t.Fatalf("refused after the first was removed: %v", err)
	}
	if err := b.Abort(); err != nil {
		t.Fatal(err)
	}

	// Another volume has its own free space.
	c, err := sink.Prepare(ctx, spec("cccccccccccccccc", "c.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Abort()
	sink.Volume = func(string) (string, error) { return "w", nil }
	d, err := sink.Prepare(ctx, spec("dddddddddddddddd", "d.bin"))
	if err != nil {
		t.Fatalf("another volume was refused: %v", err)
	}
	_ = d.Abort()
}

// TestPreallocatedFileReservesNothing: a file that took its clusters at Create
// is already out of the free space, so counting it again would refuse
// downloads that fit.
func TestPreallocatedFileReservesNothing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("preallocate allocates clusters only on NTFS")
	}
	dir := t.TempDir()
	sink := engine.NewFileSink(mustStrategy(t, fsx.StrategyPreallocate))
	fixedSpace(sink, 200*mib)
	w, err := sink.Prepare(context.Background(), engine.SinkSpec{JobID: "aaaaaaaaaaaaaaaa", DestPath: filepath.Join(dir, "a.bin"), Size: 100 * mib})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	if got := sink.Reserved("v"); got != 0 {
		t.Fatalf("a preallocated file reserved %d", got)
	}
}

// TestParallelDownloadsToOneDriveAreAdmittedInTurn runs F1 through the engine:
// with room for one object at a time, the second download fails before it
// fetches anything instead of filling the drive.
func TestParallelDownloadsToOneDriveAreAdmittedInTurn(t *testing.T) {
	dir := t.TempDir()
	sink := engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))
	fixedSpace(sink, testSize+engine.FreeSpaceReserve+testSize/2)
	h := newHarness(t, options{sink: sink})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.SlowBody(8 * mib)})
	first := h.download(filepath.Join(dir, "a.bin"))
	h.waitFor(first, "bytes", func(j engine.JobSnapshot) bool { return j.DoneBytes > 0 })
	second := h.waitState(h.download(filepath.Join(dir, "b.bin")), engine.JobFailed)
	if second.Err == nil || !strings.Contains(second.Err.Message, "insufficient disk space") || second.DoneBytes != 0 {
		t.Fatalf("second download: %+v %+v", second, second.Err)
	}
	h.server.ClearRules()
	h.waitState(first, engine.JobCompleted)
	assertSingleFile(t, dir, "a.bin")
}

// stuckSink wraps a FileSink so that, while stuck is set, Abort fails and
// leaves the temp file, as when another program holds it open.
type stuckSink struct {
	*engine.FileSink
	stuck atomic.Bool
}

func (s *stuckSink) Prepare(ctx context.Context, spec engine.SinkSpec) (engine.SinkWriter, error) {
	w, err := s.FileSink.Prepare(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &stuckWriter{SinkWriter: w, sink: s, temp: fsx.TempPath(spec.DestPath, string(spec.JobID))}, nil
}

type stuckWriter struct {
	engine.SinkWriter
	sink *stuckSink
	temp string
}

// Abort closes the file but, while stuck, leaves it on disk and fails.
func (w *stuckWriter) Abort() error {
	err := w.SinkWriter.Abort()
	if err != nil || !w.sink.stuck.Load() {
		return err
	}
	if err := os.WriteFile(w.temp, []byte("left behind"), 0o666); err != nil {
		return err
	}
	return errors.New("the file is in use by another process")
}

// TestRetryAfterTempFileWasLeftBehind is F3: a temp file that cleanup could not
// remove must not stop the job from being retried.
func TestRetryAfterTempFileWasLeftBehind(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.bin")
	sink := &stuckSink{FileSink: engine.NewFileSink(mustStrategy(t, fsx.StrategySparse))}
	sink.stuck.Store(true)
	h := newHarness(t, options{sink: sink})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.AccessDenied()})
	id := h.download(dest)
	failed := h.waitState(id, engine.JobFailed)
	if !strings.Contains(failed.Warning, "next launch") {
		t.Fatalf("no warning about the temp file: %+v", failed)
	}
	if _, err := os.Stat(fsx.TempPath(dest, string(id))); err != nil {
		t.Fatalf("the temp file should have been left: %v", err)
	}

	sink.stuck.Store(false)
	h.server.ClearRules()
	if err := h.engine.Retry(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, engine.JobCompleted)
	assertPattern(t, dest, testSize)
	assertSingleFile(t, dir, "movie.bin")
}
