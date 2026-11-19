package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
)

func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := engine.New(engine.Config{}); !errors.Is(err, engine.ErrInvalidConfig) {
		t.Fatalf("got %v", err)
	}
}

func TestCleanUploadSendsEveryPartOnce(t *testing.T) {
	h := newHarness(t, options{})
	id := h.submit()
	job := h.waitState(id, engine.JobCompleted)

	if job.DoneBytes != testSize || job.TotalBytes != testSize || job.PartsDone != testParts || job.Retries != 0 {
		t.Fatalf("final snapshot: %+v", job)
	}
	for part := int32(1); part <= testParts; part++ {
		if n := h.attempts(part); n != 1 {
			t.Errorf("part %d was sent %d times", part, n)
		}
	}
	h.assertVerified(testKey, testSeed, testSize)
	h.assertClean()

	object, _ := h.server.Object(fakes3.DefaultBucket, testKey)
	if !object.Multipart || !strings.HasSuffix(object.CRC32, fmt.Sprintf("-%d", testParts)) {
		t.Fatalf("object: %+v", object)
	}
}

func TestSinglePutBoundary(t *testing.T) {
	tests := []struct {
		name      string
		size      int64
		multipart bool
	}{
		{"zero bytes", 0, false},
		{"one byte", 1, false},
		{"at the threshold", engine.SinglePutThreshold, false},
		{"above the threshold", engine.SinglePutThreshold + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{})
			h.source.Set("file", fakes3.SyntheticFile{Size: tt.size, Seed: 9})
			id := h.submitAs("file", testRef)
			job := h.waitState(id, engine.JobCompleted)
			if job.DoneBytes != tt.size {
				t.Fatalf("done %d of %d", job.DoneBytes, tt.size)
			}
			creates := h.server.Attempts(fakes3.OpCreateUpload, fakes3.DefaultBucket, testKey, 0)
			puts := h.server.Attempts(fakes3.OpPut, fakes3.DefaultBucket, testKey, 0)
			if tt.multipart && (creates != 1 || puts != 0) || !tt.multipart && (creates != 0 || puts != 1) {
				t.Fatalf("creates %d, puts %d", creates, puts)
			}
			h.assertVerified(testKey, 9, tt.size)
			h.assertClean()
		})
	}
}

func TestSinglePutRetriesWithoutJournal(t *testing.T) {
	h := newHarness(t, options{})
	h.source.Set("file", fakes3.SyntheticFile{Size: mib, Seed: 9})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpPut, Attempt: 1, Fault: fakes3.ServiceUnavailable()})
	id := h.submitAs("file", testRef)
	if job := h.waitState(id, engine.JobCompleted); job.Retries != 1 || job.DoneBytes != mib {
		t.Fatalf("%+v", job)
	}
	h.assertVerified(testKey, 9, mib)
}

// TestInjectedPartFaults sends part 2 through each fault once. The upload must
// finish, part 2 must be sent exactly twice and no other part may be sent again.
func TestInjectedPartFaults(t *testing.T) {
	tests := []struct {
		name  string
		fault fakes3.Fault
	}{
		{"500 InternalError", fakes3.InternalError()},
		{"503 ServiceUnavailable", fakes3.ServiceUnavailable()},
		{"503 SlowDown", fakes3.SlowDown()},
		{"503 with Retry-After", fakes3.SlowDown().WithRetryAfter(time.Millisecond)},
		{"error inside a 200", fakes3.InternalError().ErrorIn200()},
		{"connection reset before the body", fakes3.ResetAt(0)},
		{"connection reset mid-body", fakes3.ResetAt(mib)},
		{"response lost after the part was stored", fakes3.LostResponse()},
		{"stall mid-body", fakes3.StallAt(mib, 0)},
		{"stall before the body", fakes3.StallAt(0, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{timeouts: func(timeouts *engine.Timeouts) {
				timeouts.Stall, timeouts.Response = 2*time.Second, 2*time.Second
			}})
			h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 2, Attempt: 1, Fault: tt.fault})
			id := h.submit()
			job := h.waitState(id, engine.JobCompleted)

			if job.Retries != 1 || job.DoneBytes != testSize {
				t.Errorf("retries %d, done %d", job.Retries, job.DoneBytes)
			}
			for part := int32(1); part <= testParts; part++ {
				want := 1
				if part == 2 {
					want = 2
				}
				if n := h.attempts(part); n != want {
					t.Errorf("part %d was sent %d times, want %d", part, n, want)
				}
			}
			h.assertVerified(testKey, testSeed, testSize)
			h.assertClean()
		})
	}
}

func TestPermanentPartFailureAbortsUpload(t *testing.T) {
	h := newHarness(t, options{})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 3, Fault: fakes3.Fault{
		Kind: fakes3.FaultError, Status: http.StatusForbidden, Code: "AccessDenied", Message: "denied",
	}})
	id := h.submit()
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassPermanent || job.Err.Code != "AccessDenied" || job.Err.PartNumber != 3 {
		t.Fatalf("error: %+v", job.Err)
	}
	if n := h.attempts(3); n != 1 {
		t.Errorf("a permanent failure was retried: %d attempts", n)
	}
	h.assertClean()
}

func TestRetryBudgetExhaustedFailsAndAborts(t *testing.T) {
	h := newHarness(t, options{})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 5, Fault: fakes3.ServiceUnavailable()})
	id := h.submit()
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassRetryable || job.Err.PartNumber != 5 {
		t.Fatalf("error: %+v", job.Err)
	}
	if n := h.attempts(5); n != 3 {
		t.Errorf("part 5 had %d attempts, want the limit of 3", n)
	}
	h.assertClean()
}

func TestOutageToleranceKeepsRetryingPastAttemptLimit(t *testing.T) {
	h := newHarness(t, options{retry: func(retry *engine.RetryPolicy) { retry.OutageTolerance = time.Hour }})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 5, Times: 8, Fault: fakes3.ServiceUnavailable()})
	id := h.submit()
	h.waitState(id, engine.JobCompleted)
	if n := h.attempts(5); n != 9 {
		t.Errorf("part 5 had %d attempts, want 9", n)
	}
	h.assertVerified(testKey, testSeed, testSize)
}

func TestControlRequestFaultsAreRetried(t *testing.T) {
	for _, op := range []fakes3.Op{fakes3.OpCreateUpload, fakes3.OpComplete} {
		t.Run(string(op), func(t *testing.T) {
			h := newHarness(t, options{})
			h.server.AddRule(fakes3.Rule{Op: op, Attempt: 1, Fault: fakes3.ServiceUnavailable()})
			id := h.submit()
			h.waitState(id, engine.JobCompleted)
			h.assertVerified(testKey, testSeed, testSize)
			h.assertClean()
		})
	}
}

func TestLostCompleteResponseIsRecovered(t *testing.T) {
	h := newHarness(t, options{})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Attempt: 1, Fault: fakes3.LostResponse()})
	id := h.submit()
	h.waitState(id, engine.JobCompleted)
	if n := h.server.Attempts(fakes3.OpHead, fakes3.DefaultBucket, testKey, 0); n != 1 {
		t.Errorf("HEAD was sent %d times", n)
	}
	h.assertVerified(testKey, testSeed, testSize)
	h.assertClean()
}

func TestLostCompleteWithCredentialExpiryWaitsThenRecovers(t *testing.T) {
	h := newHarness(t, options{})
	h.prober.failures.Store(2)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Attempt: 1, Fault: fakes3.LostResponse()})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpHead, Attempt: 1, Fault: fakes3.ExpiredToken()})
	id := h.submit()
	h.waitState(id, engine.JobCompleted)
	if h.prober.calls.Load() < 3 {
		t.Errorf("probes: %d", h.prober.calls.Load())
	}
	h.assertClean()
}

func TestFirstCompleteNoSuchUploadFails(t *testing.T) {
	h := newHarness(t, options{})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Fault: fakes3.Fault{
		Kind: fakes3.FaultError, Status: http.StatusNotFound, Code: "NoSuchUpload", Message: "gone",
	}})
	id := h.submit()
	h.waitState(id, engine.JobFailed)
	if n := h.server.Attempts(fakes3.OpHead, fakes3.DefaultBucket, testKey, 0); n != 0 {
		t.Errorf("HEAD was sent %d times after a first Complete", n)
	}
	h.assertClean()
}

// TestLostCreateResponseIsJournaled covers an upload whose ID never reached us.
// It cannot be aborted now, so its intent stays for launch cleanup to find.
func TestLostCreateResponseIsJournaled(t *testing.T) {
	h := newHarness(t, options{})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpCreateUpload, Attempt: 1, Fault: fakes3.LostResponse()})
	id := h.submit()
	h.waitState(id, engine.JobCompleted)
	h.assertVerified(testKey, testSeed, testSize)

	orphans := h.server.ActiveUploads(fakes3.DefaultBucket)
	entries := h.journal.List()
	if len(orphans) != 1 || len(entries) != 1 {
		t.Fatalf("uploads %+v, entries %+v", orphans, entries)
	}
	remote := journal.RemoteUpload{Key: orphans[0].Key, UploadID: orphans[0].UploadID, Initiated: orphans[0].Created}
	if !journal.MatchesIntent(entries[0], remote) {
		t.Fatalf("launch cleanup would not find the orphan: %+v", entries[0])
	}
}

func TestExpiredCredentialsWaitAndResume(t *testing.T) {
	gate := (*gateStore)(nil)
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		close(gate.gate)
		return gate
	}})
	h.prober.failures.Store(1 << 30) // No probe passes until the test allows it.
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 4, Attempt: 1, Fault: fakes3.ExpiredToken()})
	id := h.submit()
	job := h.waitState(id, engine.JobCredentialWait)
	if job.Err != nil {
		t.Fatalf("the job recorded an error: %+v", job.Err)
	}
	if len(h.server.ActiveUploads(fakes3.DefaultBucket)) != 1 {
		t.Fatal("the upload must survive the credential wait")
	}
	h.waitFor(id, "several probes", func(engine.JobSnapshot) bool { return h.prober.calls.Load() >= 2 })
	if ref := h.prober.last.Load(); ref == nil || *ref != testRef {
		t.Fatalf("the probe must be signed for the waiting job's object: %+v", ref)
	}

	h.prober.failures.Store(0)
	h.engine.NotifyCredentialsChanged(testRef.Profile)
	job = h.waitState(id, engine.JobCompleted)
	if job.Retries != 0 {
		t.Errorf("a credential failure was charged as a retry: %d", job.Retries)
	}
	for part := int32(1); part <= testParts; part++ {
		want := 1
		if part == 4 {
			want = 2
		}
		if n := h.attempts(part); n != want {
			t.Errorf("part %d was sent %d times, want %d", part, n, want)
		}
	}
	h.assertVerified(testKey, testSeed, testSize)
	h.assertClean()
}

func TestCancelAbortsUpload(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}})
	id := h.submit()
	gate.waitCurrent(t, engine.DefaultPartsPerFile)
	gate.release(t, 3)
	h.waitFor(id, "finished parts", func(job engine.JobSnapshot) bool { return job.PartsDone >= 3 })

	if err := h.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	job := h.waitState(id, engine.JobCancelled)
	if job.Err != nil || job.Warning != "" {
		t.Fatalf("%+v", job)
	}
	if h.server.Attempts(fakes3.OpAbort, fakes3.DefaultBucket, testKey, 0) != 1 {
		t.Error("abort was not sent exactly once")
	}
	h.assertClean()
	if err := h.engine.Cancel(context.Background(), id); !errors.Is(err, engine.ErrIllegalState) {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestCancelWhileQueuedAndPaused(t *testing.T) {
	h := newHarness(t, options{settings: func(s *engine.Settings) { s.MaxActiveFiles = 1 }})
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: "s4browser-test/run1/first", Fault: fakes3.StallAt(1, 0)})
	first := h.submitAs(testPath, engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "s4browser-test/run1/first"})
	h.waitState(first, engine.JobRunning)

	queued := h.submit()
	paused := h.submitAs(testPath, engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "s4browser-test/run1/second"})
	if err := h.engine.Pause(context.Background(), paused); err != nil {
		t.Fatal(err)
	}
	h.waitState(paused, engine.JobPaused)
	for _, id := range []engine.JobID{queued, paused, first} {
		if err := h.engine.Cancel(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		h.waitState(id, engine.JobCancelled)
	}
	h.assertClean()
}

func TestCancelDuringCreateAbortsTheNewUpload(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		return createHook{inner, entered, release}
	}})
	id := h.submit()
	<-entered
	if err := h.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	close(release)
	h.waitState(id, engine.JobCancelled)
	h.assertClean()
}

type createHook struct {
	engine.ObjectStore
	entered, release chan struct{}
}

func (c createHook) CreateMultipartUpload(ctx context.Context, ref engine.ObjectRef) (string, error) {
	close(c.entered)
	<-c.release
	return c.ObjectStore.CreateMultipartUpload(ctx, ref)
}

func TestAbortFailureKeepsJournalEntry(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}})
	rule := h.server.AddRule(fakes3.Rule{Op: fakes3.OpAbort, Fault: fakes3.ServiceUnavailable()})
	id := h.submit()
	gate.waitCurrent(t, engine.DefaultPartsPerFile)
	if err := h.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	job := h.waitState(id, engine.JobCancelled)
	if job.Warning == "" {
		t.Error("no warning about the deferred abort")
	}
	if n := h.server.RuleFired(rule); n != 3 {
		t.Errorf("abort attempts: %d, want the limit of 3", n)
	}
	entries := h.journal.List()
	if len(entries) != 1 || entries[0].Phase != journal.PhaseActive || entries[0].UploadID == "" {
		t.Fatalf("journal: %+v", entries)
	}
}

func TestDeferredCancelDuringComplete(t *testing.T) {
	t.Run("complete succeeds", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
			return completeHook{inner, func(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
				close(entered)
				<-release
				return inner.CompleteMultipartUpload(ctx, req)
			}}
		}})
		id := h.submit()
		<-entered
		if err := h.engine.Pause(context.Background(), id); !errors.Is(err, engine.ErrIllegalState) {
			t.Fatalf("pause while finalizing: %v", err)
		}
		if err := h.engine.Cancel(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		close(release)
		h.waitState(id, engine.JobCompleted)
		h.assertClean()
	})
	t.Run("complete fails", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
			return completeHook{inner, func(context.Context, engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
				close(entered)
				<-release
				return engine.CompleteUploadResult{}, &engine.OpError{Op: "Complete", Class: engine.ClassPermanent, Err: errors.New("rejected")}
			}}
		}})
		id := h.submit()
		<-entered
		if err := h.engine.Cancel(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		close(release)
		h.waitState(id, engine.JobCancelled)
		h.assertClean()
	})
}

func TestChecksumMismatchFailsWithWarning(t *testing.T) {
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		return completeHook{inner, func(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
			out, err := inner.CompleteMultipartUpload(ctx, req)
			out.ChecksumCRC32 = "AAAAAA==-16"
			return out, err
		}}
	}})
	id := h.submit()
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || !strings.Contains(job.Err.Message, "checksum") || !strings.Contains(job.Warning, "exists on S3") {
		t.Fatalf("%+v %q", job.Err, job.Warning)
	}
	h.assertClean()
}

func TestSourceChangedDuringUploadFails(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}})
	id := h.submit()
	gate.waitCurrent(t, engine.DefaultPartsPerFile)
	h.source.Set(testPath, fakes3.SyntheticFile{Size: testSize, Seed: testSeed, ModTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	close(gate.gate)
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || !strings.Contains(job.Err.Message, "source file changed") {
		t.Fatalf("%+v", job.Err)
	}
	if h.server.Attempts(fakes3.OpComplete, fakes3.DefaultBucket, testKey, 0) != 0 {
		t.Error("Complete was sent for a changed source")
	}
	h.assertClean()
}

func TestLocalReadFailureIsPermanent(t *testing.T) {
	h := newHarness(t, options{})
	h.source.Set(testPath, fakes3.SyntheticFile{Size: testSize, Seed: testSeed, FailAt: 12 * mib, FailErr: errors.New("disk failed")})
	id := h.submit()
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassPermanent || !strings.Contains(job.Err.Message, "local I/O") {
		t.Fatalf("%+v", job.Err)
	}
	h.assertClean()
}

func TestMissingSourceFails(t *testing.T) {
	h := newHarness(t, options{})
	id := h.submitAs("missing", testRef)
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassPermanent {
		t.Fatalf("%+v", job.Err)
	}
	h.assertClean()
}

func TestGuardRefusesKeyBeforeAnyRequest(t *testing.T) {
	h := newHarness(t, options{})
	ref := testRef
	ref.Key = "production/object.bin"
	id := h.submitAs(testPath, ref)
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || !strings.Contains(job.Err.Message, "refused") {
		t.Fatalf("%+v", job.Err)
	}
	if n := len(h.server.Requests()); n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}
}

func TestJournalFailureStopsBeforeCreate(t *testing.T) {
	h := newHarness(t, options{journal: func(inner journal.Store) journal.Store {
		return failingJournal{Store: inner}
	}})
	id := h.submit()
	job := h.waitState(id, engine.JobFailed)
	if job.Err == nil || !strings.Contains(job.Err.Message, "journal") {
		t.Fatalf("%+v", job.Err)
	}
	if n := h.server.Attempts(fakes3.OpCreateUpload, fakes3.DefaultBucket, testKey, 0); n != 0 {
		t.Fatalf("Create was sent %d times without a journal entry", n)
	}
}

type failingJournal struct{ journal.Store }

func (failingJournal) Put(context.Context, journal.Entry) error { return errors.New("disk full") }

func TestJournalActiveFailureAbortsUpload(t *testing.T) {
	h := newHarness(t, options{journal: func(inner journal.Store) journal.Store {
		return &failActiveJournal{Store: inner}
	}})
	id := h.submit()
	h.waitState(id, engine.JobFailed)
	if h.server.Attempts(fakes3.OpAbort, fakes3.DefaultBucket, testKey, 0) != 1 {
		t.Error("the upload was not aborted")
	}
	h.assertClean()
}

type failActiveJournal struct{ journal.Store }

func (f *failActiveJournal) Put(ctx context.Context, entry journal.Entry) error {
	if entry.Phase == journal.PhaseActive {
		return errors.New("disk full")
	}
	return f.Store.Put(ctx, entry)
}

func TestRetryFailedJobStartsFromNothing(t *testing.T) {
	h := newHarness(t, options{})
	rule := h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 3, Fault: fakes3.Fault{
		Kind: fakes3.FaultError, Status: http.StatusForbidden, Code: "AccessDenied", Message: "denied",
	}})
	id := h.submit()
	h.waitState(id, engine.JobFailed)
	h.server.RemoveRule(rule)

	if err := h.engine.Retry(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	job := h.waitFor(id, "completion", func(job engine.JobSnapshot) bool { return job.State == engine.JobCompleted })
	if job.DoneBytes != testSize || job.Err != nil {
		t.Fatalf("%+v", job)
	}
	if n := h.server.Attempts(fakes3.OpCreateUpload, fakes3.DefaultBucket, testKey, 0); n != 2 {
		t.Errorf("Create was sent %d times, want a new upload", n)
	}
	h.assertVerified(testKey, testSeed, testSize)
	h.assertClean()

	if err := h.engine.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Remove(context.Background(), id); !errors.Is(err, engine.ErrUnknownJob) {
		t.Fatalf("second remove: %v", err)
	}
}

func TestPauseKeepsFinishedPartsAndResumes(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}, settings: func(s *engine.Settings) { s.MaxPartsPerFile = 2 }})
	id := h.submit()
	gate.waitCurrent(t, 2)
	gate.release(t, 4)
	h.waitFor(id, "four parts", func(job engine.JobSnapshot) bool { return job.PartsDone >= 4 })

	if err := h.engine.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	paused := h.waitState(id, engine.JobPaused)
	gate.waitCurrent(t, 0)
	if len(h.server.ActiveUploads(fakes3.DefaultBucket)) != 1 {
		t.Fatal("pause must keep the upload")
	}
	if paused.DoneBytes != int64(paused.PartsDone)*testPartSize {
		t.Errorf("paused with %d bytes for %d parts", paused.DoneBytes, paused.PartsDone)
	}

	close(gate.gate)
	if err := h.engine.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, engine.JobCompleted)
	for part := int32(1); part <= testParts; part++ {
		if n := h.attempts(part); n != 1 {
			t.Errorf("part %d reached the store %d times", part, n)
		}
	}
	h.assertVerified(testKey, testSeed, testSize)
	h.assertClean()
}

func TestProgressBatchesAreOrderedAndFinal(t *testing.T) {
	h := newHarness(t, options{settings: func(s *engine.Settings) { s.UploadBytesPerSec = 100 * mib }})
	id := h.submit()
	h.waitState(id, engine.JobCompleted)
	h.close()

	batches := h.obs.all()
	if len(batches) < 2 {
		t.Fatalf("only %d batches", len(batches))
	}
	var last engine.JobSnapshot
	var states []engine.JobState
	for i, batch := range batches {
		if batch.Seq != uint64(i+1) {
			t.Fatalf("batch %d has Seq %d", i, batch.Seq)
		}
		for _, job := range batch.Jobs {
			last = job
		}
		for _, transition := range batch.Transitions {
			states = append(states, transition.To)
		}
	}
	if last.State != engine.JobCompleted || last.DoneBytes != testSize {
		t.Fatalf("last report: %+v", last)
	}
	if len(states) != 2 || states[0] != engine.JobRunning || states[1] != engine.JobCompleted {
		t.Fatalf("transitions: %v", states)
	}
}

// ---- Scheduling limits and live settings ----

func TestGlobalAndPerFileLimitsHold(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}, settings: func(s *engine.Settings) {
		s.MaxActiveFiles, s.MaxPartsPerFile, s.MaxActiveParts = 2, 3, 4
	}})
	var ids []engine.JobID
	for i := range 3 {
		ref := testRef
		ref.Key = fmt.Sprintf("s4browser-test/run1/file-%d", i)
		ids = append(ids, h.submitAs(testPath, ref))
	}
	gate.waitCurrent(t, 4)
	time.Sleep(50 * time.Millisecond)
	if current, _, _ := gate.stats(); current != 4 {
		t.Fatalf("%d parts in flight, global limit is 4", current)
	}
	if third := h.snapshot(ids[2]); third.State != engine.JobQueued {
		t.Fatalf("third file is %s, active-file limit is 2", third.State)
	}
	close(gate.gate)
	for _, id := range ids {
		h.waitState(id, engine.JobCompleted)
	}
	if _, peak, _ := gate.stats(); peak > 4 {
		t.Fatalf("peak %d parts", peak)
	}
}

func TestLiveConcurrencyChanges(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}, settings: func(s *engine.Settings) { s.MaxPartsPerFile = 2 }})
	id := h.submit()
	gate.waitCurrent(t, 2)

	settings := engine.DefaultSettings()
	settings.PartSize = testPartSize
	settings.MaxPartsPerFile = 8
	if err := h.engine.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	gate.waitCurrent(t, 8) // Raising starts work at once, with nothing finishing.

	settings.MaxPartsPerFile = 1
	if err := h.engine.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if current, _, _ := gate.stats(); current != 8 {
		t.Fatalf("lowering the limit interrupted attempts: %d in flight", current)
	}
	gate.release(t, 8)
	gate.waitCurrent(t, 1) // The count falls as attempts finish.
	gate.resetPeak()
	close(gate.gate)
	h.waitState(id, engine.JobCompleted)
	if _, peak, _ := gate.stats(); peak != 1 {
		t.Fatalf("peak %d after lowering the limit to 1", peak)
	}
	if job := h.snapshot(id); job.Retries != 0 {
		t.Fatalf("settings changes caused %d retries", job.Retries)
	}
	h.assertVerified(testKey, testSeed, testSize)

	settings.MaxActiveParts = 0
	if err := h.engine.UpdateSettings(context.Background(), settings); !errors.Is(err, engine.ErrInvalidSettings) {
		t.Fatalf("invalid settings: %v", err)
	}
}

func TestLiveActiveFileLimitChange(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}, settings: func(s *engine.Settings) { s.MaxActiveFiles, s.MaxPartsPerFile = 1, 1 }})
	for i := range 3 {
		ref := testRef
		ref.Key = fmt.Sprintf("s4browser-test/run1/file-%d", i)
		h.submitAs(testPath, ref)
	}
	gate.waitCurrent(t, 1)
	settings := engine.DefaultSettings()
	settings.PartSize = testPartSize
	settings.MaxActiveFiles, settings.MaxPartsPerFile = 3, 1
	if err := h.engine.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	gate.waitCurrent(t, 3)
	close(gate.gate)
}

func TestLiveBandwidthChange(t *testing.T) {
	const slow, size = 40 * mib, 240 * mib
	h := newHarness(t, options{settings: func(s *engine.Settings) { s.UploadBytesPerSec = slow }})
	h.source.Set("big", fakes3.SyntheticFile{Size: size, Seed: testSeed})
	id := h.submitAs("big", testRef)

	h.waitFor(id, "the first bytes", func(job engine.JobSnapshot) bool { return job.DoneBytes > 16*mib })
	start, startBytes := time.Now(), h.snapshot(id).DoneBytes
	time.Sleep(time.Second)
	elapsed, moved := time.Since(start).Seconds(), h.snapshot(id).DoneBytes-startBytes
	measured := float64(moved) / elapsed
	// In-flight accounting adds up to one quantum per part, hence the margin.
	if measured > 1.5*float64(slow) {
		t.Fatalf("limited to %d MiB/s but measured %.0f MiB/s", slow/mib, measured/float64(mib))
	}

	settings := engine.DefaultSettings()
	settings.PartSize = testPartSize
	settings.UploadBytesPerSec = 0
	if err := h.engine.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	remaining := float64(size - h.snapshot(id).DoneBytes)
	raised := time.Now()
	h.waitState(id, engine.JobCompleted)
	if atOldRate := remaining / float64(slow); time.Since(raised).Seconds() > atOldRate/2 {
		t.Fatalf("removing the limit took %v for a remainder of %.1f s at the old rate", time.Since(raised), atOldRate)
	}
	h.assertVerified(testKey, testSeed, size)
}

func TestPartSizeChangeAppliesToUnplannedJobs(t *testing.T) {
	h := newHarness(t, options{})
	settings := engine.DefaultSettings()
	settings.PartSize = 10 * mib
	if err := h.engine.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	id := h.submit()
	if job := h.waitState(id, engine.JobCompleted); job.PartsTotal != 8 {
		t.Fatalf("%d parts, want 8 of 10 MiB", job.PartsTotal)
	}
}

// ---- Shutdown ----

func TestCloseDuringTransferAbortsAndLeavesNothing(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}})
	id := h.submit()
	gate.waitCurrent(t, engine.DefaultPartsPerFile)
	queued := h.submitAs(testPath, engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "s4browser-test/run1/queued"})

	h.close()

	h.assertClean()
	ctx := context.Background()
	if _, err := h.engine.Submit(ctx, engine.JobSpec{Direction: engine.Upload, Ref: testRef}); !errors.Is(err, engine.ErrEngineClosed) {
		t.Errorf("Submit after Close: %v", err)
	}
	if err := h.engine.Cancel(ctx, id); !errors.Is(err, engine.ErrEngineClosed) {
		t.Errorf("Cancel after Close: %v", err)
	}
	if err := h.engine.Pause(ctx, queued); !errors.Is(err, engine.ErrEngineClosed) {
		t.Errorf("Pause after Close: %v", err)
	}
	if _, err := h.engine.Snapshot(ctx); !errors.Is(err, engine.ErrEngineClosed) {
		t.Errorf("Snapshot after Close: %v", err)
	}
	if err := h.engine.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCloseCancelsCompleteAndKeepsJournalEntry(t *testing.T) {
	entered := make(chan struct{})
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		return completeHook{inner, func(ctx context.Context, _ engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
			close(entered)
			<-ctx.Done()
			return engine.CompleteUploadResult{}, context.Cause(ctx)
		}}
	}})
	h.submit()
	<-entered
	h.close()
	entries := h.journal.List()
	if len(entries) != 1 || entries[0].Phase != journal.PhaseActive {
		t.Fatalf("journal: %+v", entries)
	}
	if n := h.server.Attempts(fakes3.OpAbort, fakes3.DefaultBucket, testKey, 0); n != 0 {
		t.Fatalf("abort was sent %d times while Complete's outcome was unknown", n)
	}
}

func TestCloseGivesUpOnAbortsAfterDeadline(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return hangingAbort{gate}
	}, timeouts: func(timeouts *engine.Timeouts) { timeouts.ShutdownAbort = 100 * time.Millisecond }})
	h.submit()
	gate.waitCurrent(t, engine.DefaultPartsPerFile)
	start := time.Now()
	h.close()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	if entries := h.journal.List(); len(entries) != 1 {
		t.Fatalf("the unaborted upload must stay journaled: %+v", entries)
	}
}

type hangingAbort struct{ engine.ObjectStore }

func (hangingAbort) AbortMultipartUpload(ctx context.Context, _ engine.ObjectRef, _ string) error {
	<-ctx.Done()
	return context.Cause(ctx)
}

func TestCloseHonoursCallerContext(t *testing.T) {
	release := make(chan struct{})
	var once atomic.Bool
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		return stuckPart{inner, release, &once}
	}})
	h.submit()
	h.waitFor(h.firstJob(), "a stuck part", func(engine.JobSnapshot) bool { return once.Load() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.engine.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	close(release)
}

func (h *harness) firstJob() engine.JobID {
	h.t.Helper()
	all, err := h.engine.Snapshot(context.Background())
	if err != nil || len(all) == 0 {
		h.t.Fatalf("no jobs: %v", err)
	}
	return all[0].ID
}

// stuckPart ignores cancellation until released, like a store that breaks its contract.
type stuckPart struct {
	engine.ObjectStore
	release chan struct{}
	entered *atomic.Bool
}

func (s stuckPart) UploadPart(context.Context, engine.UploadPartRequest) (engine.PutResult, error) {
	s.entered.Store(true)
	<-s.release
	return engine.PutResult{}, errors.New("released")
}

// ---- Journal ----

// TestSecondInstanceCannotTakeOverDuringUpload proves the lock holds while an
// upload is live, so a second app could never abort it.
func TestSecondInstanceCannotTakeOverDuringUpload(t *testing.T) {
	var gate *gateStore
	h := newHarness(t, options{wrap: func(inner engine.ObjectStore) engine.ObjectStore {
		gate = newGateStore(inner)
		return gate
	}})
	id := h.submit()
	gate.waitCurrent(t, engine.DefaultPartsPerFile)

	if second, err := journal.OpenFile(h.dir); !errors.Is(err, journal.ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second instance: got %v, want ErrLocked", err)
	}
	close(gate.gate)
	h.waitState(id, engine.JobCompleted)
	h.assertVerified(testKey, testSeed, testSize)
}

// snapshotJournal copies the journal file's bytes after every change.
type snapshotJournal struct {
	journal.Store
	dir   string
	files atomic.Pointer[[][]byte]
}

func (s *snapshotJournal) Put(ctx context.Context, entry journal.Entry) error {
	if err := s.Store.Put(ctx, entry); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, journal.FileName))
	if err != nil {
		return err
	}
	var next [][]byte
	if previous := s.files.Load(); previous != nil {
		next = append(next, *previous...)
	}
	next = append(next, data)
	s.files.Store(&next)
	return nil
}

func TestJournalEntriesContainNoSecrets(t *testing.T) {
	var recorder *snapshotJournal
	dirOf := func(store journal.Store) journal.Store {
		recorder = &snapshotJournal{Store: store}
		return recorder
	}
	h := newHarness(t, options{journal: dirOf})
	recorder.dir = h.dir
	id := h.submit()
	h.waitState(id, engine.JobCompleted)

	files := recorder.files.Load()
	if files == nil || len(*files) < 2 {
		t.Fatal("the journal was not written")
	}
	secretShapes := regexp.MustCompile(`(?i)secret|session.?token|password|authorization|signature|AKIA|ASIA`)
	for _, data := range *files {
		if match := secretShapes.Find(data); match != nil {
			t.Fatalf("journal contains %q:\n%s", match, data)
		}
		for _, secret := range []string{fakes3.AccessKeyID, fakes3.SecretAccessKey} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("journal contains a credential:\n%s", data)
			}
		}
		if _, err := journal.Decode(data); err != nil {
			t.Fatalf("journal does not decode: %v", err)
		}
	}
}

func TestFileSourceReadsRealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.bin")
	if err := os.WriteFile(path, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	handle, err := engine.FileSource{}.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	stat, err := handle.Stat()
	if err != nil || stat.Size != 11 {
		t.Fatalf("%+v, %v", stat, err)
	}
	buf := make([]byte, 5)
	if _, err := handle.ReadAt(buf, 6); err != nil || string(buf) != "world" {
		t.Fatalf("%q, %v", buf, err)
	}
	if _, err := (engine.FileSource{}).Open(ctx, filepath.Dir(path)); err == nil {
		t.Fatal("a directory was accepted as a source")
	}
}
