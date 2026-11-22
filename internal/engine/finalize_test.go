package engine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
)

// These tests cover findings F4 and F5 of the stage 4.1 engine review (docs/review-4.1.md, removed;
// see git history).

// heldStatSource holds the second Stat of every handle, which is the check
// before Complete, until release is closed.
type heldStatSource struct {
	engine.Source
	entered chan struct{}
	release chan struct{}
}

func (s heldStatSource) Open(ctx context.Context, path string) (engine.SourceHandle, error) {
	h, err := s.Source.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return &heldStatHandle{SourceHandle: h, src: s}, nil
}

type heldStatHandle struct {
	engine.SourceHandle
	src   heldStatSource
	stats atomic.Int32
}

func (h *heldStatHandle) Stat() (engine.SourceStat, error) {
	if h.stats.Add(1) == 2 {
		close(h.src.entered)
		<-h.src.release
	}
	return h.SourceHandle.Stat()
}

// TestCancelWhileVerifyingSourceSendsNoComplete: a cancel made before Complete
// was sent must not create the object.
func TestCancelWhileVerifyingSourceSendsNoComplete(t *testing.T) {
	src := heldStatSource{entered: make(chan struct{}), release: make(chan struct{})}
	var completes atomic.Int32
	h := newHarness(t, options{
		source: func(inner engine.Source) engine.Source { src.Source = inner; return src },
		wrap: func(inner engine.ObjectStore) engine.ObjectStore {
			return completeHook{inner, func(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
				completes.Add(1)
				return inner.CompleteMultipartUpload(ctx, req)
			}}
		},
	})
	id := h.submit()
	<-src.entered
	if err := h.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	close(src.release)
	h.waitState(id, engine.JobCancelled)
	if n := completes.Load(); n != 0 {
		t.Fatalf("%d Complete requests were sent after the cancel", n)
	}
	if _, ok := h.server.Object(fakes3.DefaultBucket, testKey); ok {
		t.Fatal("the object was created")
	}
	h.assertClean()
}

// TestCancelWhileCompleteWaitsForRetry: a cancel during the backoff between
// Complete tries applies at once instead of after the outage tolerance.
func TestCancelWhileCompleteWaitsForRetry(t *testing.T) {
	failed := make(chan struct{})
	var completes atomic.Int32
	h := newHarness(t, options{
		retry: func(r *engine.RetryPolicy) {
			r.BaseDelay, r.MaxDelay, r.OutageTolerance = time.Hour, time.Hour, time.Hour
		},
		wrap: func(inner engine.ObjectStore) engine.ObjectStore {
			return completeHook{inner, func(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
				if completes.Add(1) == 1 {
					defer close(failed)
				}
				return engine.CompleteUploadResult{}, &engine.OpError{Op: "Complete", Class: engine.ClassRetryable, Status: 503, Err: errors.New("unavailable")}
			}}
		},
	})
	id := h.submit()
	<-failed
	h.waitFor(id, "the retry", func(j engine.JobSnapshot) bool { return j.Retries == 1 })
	if err := h.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	h.waitState(id, engine.JobCancelled)
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("the cancel took %v", waited)
	}
	if n := completes.Load(); n != 1 {
		t.Fatalf("%d Complete requests, want 1", n)
	}
	h.assertClean()
}

// TestDownloadFinishesThroughRepeatedResets: every GET is reset after 1 MiB,
// so each 5 MiB part needs 5 attempts, more than the 3 the policy allows. Each
// attempt writes bytes the next one resumes after, so none counts against the
// budget and the download finishes.
func TestDownloadFinishesThroughRepeatedResets(t *testing.T) {
	h := newHarness(t, options{})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.ResetAt(mib)})
	id := h.download(dlPath)
	job := h.waitState(id, engine.JobCompleted)
	if job.Retries < testParts*4 {
		t.Fatalf("only %d retries, the fault did not fire as planned", job.Retries)
	}
	h.assertWrittenOnce(dlPath, testSize)
	h.assertClean()
}

// TestDownloadWithoutProgressStillFails keeps the budget meaningful: a reset
// before any byte arrives is charged.
func TestDownloadWithoutProgressStillFails(t *testing.T) {
	h := newHarness(t, options{})
	h.putObject(dlKey, testSize)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: dlKey, Fault: fakes3.ResetAt(0)})
	job := h.waitState(h.download(dlPath), engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassRetryable {
		t.Fatalf("%+v", job.Err)
	}
	h.assertClean()
}

// TestCredentialWaitDoesNotBlockOtherProfiles is review finding F7: with one
// file at a time, a job waiting for profile "work" credentials must not stop a
// later "home" job. The queued "work" job behind it waits for the credentials
// instead of being admitted only to join the wait.
func TestCredentialWaitDoesNotBlockOtherProfiles(t *testing.T) {
	h := newHarness(t, options{settings: func(s *engine.Settings) { s.MaxActiveFiles = 1 }})
	work := func(name string) engine.ObjectRef {
		return engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "s4browser-test/run1/" + name}
	}
	home := engine.ObjectRef{Profile: "home", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "s4browser-test/run1/home"}
	h.prober.failures.Store(1 << 30)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: work("first").Key, Fault: fakes3.ExpiredToken()})

	first := h.submitAs(testPath, work("first"))
	h.waitState(first, engine.JobCredentialWait)
	second := h.submitAs(testPath, work("second"))
	other := h.submitAs(testPath, home)

	h.waitState(other, engine.JobCompleted)
	if s := h.snapshot(second); s.State != engine.JobQueued {
		t.Fatalf("the second work job was admitted during the wait: %s", s.State)
	}
	if s := h.snapshot(first); s.State != engine.JobCredentialWait {
		t.Fatalf("first: %s", s.State)
	}

	h.server.ClearRules()
	h.prober.failures.Store(0)
	h.engine.NotifyCredentialsChanged("work")
	h.waitState(first, engine.JobCompleted)
	h.waitState(second, engine.JobCompleted)
	h.assertVerified(work("first").Key, testSeed, testSize)
	h.assertVerified(work("second").Key, testSeed, testSize)
	h.assertClean()
}

// TestHeldJobsReturnWhenTheWaitEndsWithoutCredentials: if the waiting job is
// cancelled instead, jobs held behind it are admitted.
func TestHeldJobsReturnWhenTheWaitEndsWithoutCredentials(t *testing.T) {
	h := newHarness(t, options{settings: func(s *engine.Settings) { s.MaxActiveFiles = 1 }})
	ref := func(name string) engine.ObjectRef {
		return engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "s4browser-test/run1/" + name}
	}
	h.prober.failures.Store(1 << 30)
	h.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: ref("first").Key, Fault: fakes3.ExpiredToken()})
	first := h.submitAs(testPath, ref("first"))
	h.waitState(first, engine.JobCredentialWait)
	second := h.submitAs(testPath, ref("second"))
	h.waitFor(second, "held", func(j engine.JobSnapshot) bool { return j.State == engine.JobQueued })
	if err := h.engine.Cancel(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	h.waitState(first, engine.JobCancelled)
	h.waitState(second, engine.JobCompleted)
	h.assertVerified(ref("second").Key, testSeed, testSize)
	h.assertClean()
}
