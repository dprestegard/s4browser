package s3x_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

// These tests drive the real SDK against the local TLS fake. No AWS call is made.

const (
	mib     = int64(1) << 20
	key     = "s4browser-test/run1/object.bin"
	seed    = uint64(0xabc)
	size    = 80 * mib
	parts   = 16
	srcPath = "source.bin"
)

var ref = engine.ObjectRef{Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: key}

func newServer(t *testing.T) *fakes3.Server {
	t.Helper()
	baseline := runtime.NumGoroutine()
	server, err := fakes3.New(fakes3.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Close()
		deadline := time.Now().Add(15 * time.Second)
		for runtime.NumGoroutine() > baseline {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Fatalf("goroutines: %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return server
}

func newStore(server *fakes3.Server) *s3x.Store {
	return s3x.NewStore(s3x.StaticClient(server.S3Client()))
}

type passProber struct{}

func (passProber) Probe(context.Context, engine.ObjectRef) error { return nil }

type rig struct {
	t       *testing.T
	server  *fakes3.Server
	engine  engine.Engine
	journal *journal.FileStore
}

func newRig(t *testing.T, source engine.Source, tune func(*engine.RetryPolicy, *engine.Timeouts)) *rig {
	t.Helper()
	server := newServer(t)
	store, err := journal.OpenFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	retry := engine.DefaultRetryPolicy()
	retry.BaseDelay, retry.ThrottleBaseDelay, retry.MaxDelay = time.Millisecond, time.Millisecond, 5*time.Millisecond
	retry.OutageTolerance = 0
	timeouts := engine.DefaultTimeouts()
	timeouts.ProbeMin, timeouts.ProbeMax = 5*time.Millisecond, 20*time.Millisecond
	if tune != nil {
		tune(&retry, &timeouts)
	}
	settings := engine.DefaultSettings()
	settings.PartSize = 5 * mib
	eng, err := engine.New(engine.Config{
		Store: newStore(server), Source: source, Sink: fakes3.NewSink(func(string) uint64 { return seed }),
		Journal: store, Prober: passProber{}, Clock: engine.RealClock{},
		Jitter: func() float64 { return 0.5 }, Settings: settings, Retry: retry, Timeouts: timeouts,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := eng.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return &rig{t: t, server: server, engine: eng, journal: store}
}

func syntheticSource() *fakes3.Source {
	source := fakes3.NewSource()
	source.Set(srcPath, fakes3.SyntheticFile{Size: size, Seed: seed})
	return source
}

func (r *rig) submit() engine.JobID {
	r.t.Helper()
	id, err := r.engine.Submit(context.Background(), engine.JobSpec{Direction: engine.Upload, Ref: ref, LocalPath: srcPath})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *rig) wait(id engine.JobID, want engine.JobState) engine.JobSnapshot {
	r.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		all, err := r.engine.Snapshot(context.Background())
		if err != nil {
			r.t.Fatal(err)
		}
		job := all[0]
		if job.ID == id && job.State == want {
			return job
		}
		if job.State.IsTerminal() || time.Now().After(deadline) {
			r.t.Fatalf("job is %s, want %s: %+v", job.State, want, job.Err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *rig) assertClean() {
	r.t.Helper()
	if uploads := r.server.ActiveUploads(fakes3.DefaultBucket); len(uploads) != 0 {
		r.t.Errorf("multipart uploads remain: %+v", uploads)
	}
	if entries := r.journal.List(); len(entries) != 0 {
		r.t.Errorf("journal entries remain: %+v", entries)
	}
}

func TestEngineUploadOverHTTPS(t *testing.T) {
	r := newRig(t, syntheticSource(), nil)
	job := r.wait(r.submit(), engine.JobCompleted)
	if job.DoneBytes != size || job.PartsDone != parts {
		t.Fatalf("%+v", job)
	}
	for part := int32(1); part <= parts; part++ {
		if n := r.server.Attempts(fakes3.OpUploadPart, fakes3.DefaultBucket, key, part); n != 1 {
			t.Errorf("part %d was sent %d times", part, n)
		}
	}
	if err := r.server.VerifyObject(fakes3.DefaultBucket, key, seed, 1); err != nil {
		t.Fatal(err)
	}
	r.assertClean()
}

// TestEngineUploadsRealFile covers the production FileSource end to end.
func TestEngineUploadsRealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "real.bin")
	data := make([]byte, 70*mib)
	fakes3.Pattern{Seed: seed}.Fill(data, 0)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, engine.FileSource{}, nil)
	id, err := r.engine.Submit(context.Background(), engine.JobSpec{Direction: engine.Upload, Ref: ref, LocalPath: path})
	if err != nil {
		t.Fatal(err)
	}
	r.wait(id, engine.JobCompleted)
	if err := r.server.VerifyObject(fakes3.DefaultBucket, key, seed, 1); err != nil {
		t.Fatal(err)
	}
	r.assertClean()
}

func TestSinglePutOverHTTPS(t *testing.T) {
	for name, length := range map[string]int64{"empty": 0, "small": 3 * mib} {
		t.Run(name, func(t *testing.T) {
			source := fakes3.NewSource()
			source.Set(srcPath, fakes3.SyntheticFile{Size: length, Seed: seed})
			r := newRig(t, source, nil)
			r.wait(r.submit(), engine.JobCompleted)
			if err := r.server.VerifyObject(fakes3.DefaultBucket, key, seed, 1); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPartFaultsOverHTTPS(t *testing.T) {
	tests := []struct {
		name  string
		fault fakes3.Fault
	}{
		{"503 SlowDown", fakes3.SlowDown()},
		{"500 InternalError", fakes3.InternalError()},
		{"connection reset mid-body", fakes3.ResetAt(mib)},
		{"response lost", fakes3.LostResponse()},
		{"stall mid-body", fakes3.StallAt(mib, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, syntheticSource(), func(_ *engine.RetryPolicy, timeouts *engine.Timeouts) {
				timeouts.Stall, timeouts.Response = 2*time.Second, 2*time.Second
			})
			r.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 2, Attempt: 1, Fault: tt.fault})
			job := r.wait(r.submit(), engine.JobCompleted)
			if job.Retries != 1 || job.DoneBytes != size {
				t.Errorf("retries %d, done %d", job.Retries, job.DoneBytes)
			}
			for part := int32(1); part <= parts; part++ {
				want := 1
				if part == 2 {
					want = 2
				}
				if n := r.server.Attempts(fakes3.OpUploadPart, fakes3.DefaultBucket, key, part); n != want {
					t.Errorf("part %d was sent %d times, want %d", part, n, want)
				}
			}
			if err := r.server.VerifyObject(fakes3.DefaultBucket, key, seed, 1); err != nil {
				t.Fatal(err)
			}
			r.assertClean()
		})
	}
}

func TestCancelOverHTTPSLeavesNoUpload(t *testing.T) {
	r := newRig(t, syntheticSource(), nil)
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 3, Fault: fakes3.StallAt(mib, 0)})
	id := r.submit()
	deadline := time.Now().Add(30 * time.Second)
	for len(r.server.ActiveUploads(fakes3.DefaultBucket)) == 0 || r.server.Attempts(fakes3.OpUploadPart, fakes3.DefaultBucket, key, 3) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upload never started")
		}
		time.Sleep(time.Millisecond)
	}
	if err := r.engine.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r.wait(id, engine.JobCancelled)
	r.assertClean()
}

func TestLostCompleteOverHTTPSRecoversThroughHead(t *testing.T) {
	r := newRig(t, syntheticSource(), nil)
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Attempt: 1, Fault: fakes3.LostResponse()})
	r.wait(r.submit(), engine.JobCompleted)
	if n := r.server.Attempts(fakes3.OpHead, fakes3.DefaultBucket, key, 0); n != 1 {
		t.Errorf("HEAD was sent %d times", n)
	}
	r.assertClean()
}

func accessDenied() fakes3.Fault {
	return fakes3.Fault{Kind: fakes3.FaultError, Status: http.StatusForbidden, Code: "AccessDenied", Message: "Access Denied"}
}

// TestExpiredCredentialsOnHeadWaitInsteadOfFailing is the case a bare HEAD status
// used to get wrong: lost-Complete recovery must wait for new credentials.
func TestExpiredCredentialsOnHeadWaitInsteadOfFailing(t *testing.T) {
	r := newRig(t, syntheticSource(), nil)
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Attempt: 1, Fault: fakes3.LostResponse()})
	head := r.server.AddRule(fakes3.Rule{Op: fakes3.OpHead, Fault: fakes3.ExpiredToken()})
	get := r.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Fault: fakes3.ExpiredToken()})
	id := r.submit()
	r.wait(id, engine.JobCredentialWait)
	r.server.RemoveRule(head)
	r.server.RemoveRule(get)
	r.wait(id, engine.JobCompleted)
	r.assertClean()
}

func TestDeniedHeadFailsTheJobInsteadOfWaitingForever(t *testing.T) {
	r := newRig(t, syntheticSource(), nil)
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Attempt: 1, Fault: fakes3.LostResponse()})
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpHead, Fault: accessDenied()})
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Fault: accessDenied()})
	job := r.wait(r.submit(), engine.JobFailed)
	if job.Err == nil || job.Err.Class != engine.ClassPermanent || job.Err.Code != "AccessDenied" {
		t.Fatalf("%+v", job.Err)
	}
}

func TestUndiagnosableHeadIsBoundedByRetryBudget(t *testing.T) {
	r := newRig(t, syntheticSource(), nil)
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Attempt: 1, Fault: fakes3.LostResponse()})
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpHead, Fault: accessDenied()})
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Fault: fakes3.ResetAt(0)})
	job := r.wait(r.submit(), engine.JobFailed)
	if job.Err == nil || job.Err.Code != engine.CodeAmbiguousAuth {
		t.Fatalf("%+v", job.Err)
	}
	if n := r.server.Attempts(fakes3.OpHead, fakes3.DefaultBucket, key, 0); n != engine.DefaultRetryPolicy().MaxControlAttempts {
		t.Errorf("HEAD was sent %d times", n)
	}
}

func TestHeadDiagnosis(t *testing.T) {
	expired := fakes3.ExpiredToken()
	badKey := fakes3.InvalidAccessKeyID()
	bareBadRequest := fakes3.Fault{Kind: fakes3.FaultError, Status: http.StatusBadRequest, Code: "ExpiredToken"}
	tests := []struct {
		name      string
		size      int64
		head      fakes3.Fault
		get       *fakes3.Fault
		wantClass engine.Class
		wantCode  string
		wantGets  int
	}{
		{"expired token", 10, expired, &expired, engine.ClassCredential, "ExpiredToken", 1},
		{"unknown access key", 10, badKey, &badKey, engine.ClassCredential, "InvalidAccessKeyId", 1},
		{"access denied", 10, accessDenied(), ptr(accessDenied()), engine.ClassPermanent, "AccessDenied", 1},
		{"HEAD failed but GET works", 10, bareBadRequest, nil, engine.ClassRetryable, engine.CodeAmbiguousAuth, 1},
		{"HEAD failed on an empty object", 0, accessDenied(), nil, engine.ClassRetryable, engine.CodeAmbiguousAuth, 1},
		{"diagnosis lost its connection", 10, accessDenied(), ptr(fakes3.ResetAt(0)), engine.ClassRetryable, engine.CodeAmbiguousAuth, 1},
		{"server error needs no diagnosis", 10, fakes3.ServiceUnavailable(), nil, engine.ClassRetryable, "ServiceUnavailable", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t)
			if err := server.PutSynthetic(fakes3.DefaultBucket, key, seed, tt.size); err != nil {
				t.Fatal(err)
			}
			server.AddRule(fakes3.Rule{Op: fakes3.OpHead, Fault: tt.head})
			if tt.get != nil {
				server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Fault: *tt.get})
			}
			_, err := newStore(server).HeadObject(context.Background(), ref)
			var opErr *engine.OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("got %v", err)
			}
			if opErr.Class != tt.wantClass || (tt.wantCode != "" && opErr.Code != tt.wantCode) {
				t.Fatalf("class %v code %q, want %v %q: %v", opErr.Class, opErr.Code, tt.wantClass, tt.wantCode, err)
			}
			if n := server.Attempts(fakes3.OpGet, fakes3.DefaultBucket, key, 0); n != tt.wantGets {
				t.Fatalf("%d diagnostic GETs, want %d", n, tt.wantGets)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestHeadMissingObjectIsPermanentWithoutDiagnosis(t *testing.T) {
	server := newServer(t)
	_, err := newStore(server).HeadObject(context.Background(), ref)
	var opErr *engine.OpError
	if !errors.As(err, &opErr) || opErr.Class != engine.ClassPermanent || opErr.Status != http.StatusNotFound {
		t.Fatalf("got %v", err)
	}
	if n := server.Attempts(fakes3.OpGet, fakes3.DefaultBucket, key, 0); n != 0 {
		t.Fatalf("%d diagnostic GETs", n)
	}
}

func TestErrorMapping(t *testing.T) {
	server := newServer(t)
	store := newStore(server)
	ctx := context.Background()

	server.AddRule(fakes3.Rule{Op: fakes3.OpCreateUpload, Attempt: 1, Fault: fakes3.SlowDown().WithRetryAfter(7 * time.Second)})
	_, err := store.CreateMultipartUpload(ctx, ref)
	var opErr *engine.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("got %v", err)
	}
	if opErr.Class != engine.ClassRetryable || !opErr.Throttled || opErr.Status != 503 || opErr.Code != "SlowDown" ||
		opErr.RetryAfter != 7*time.Second || opErr.RequestID == "" {
		t.Fatalf("%+v", opErr)
	}

	server.AddRule(fakes3.Rule{Op: fakes3.OpCreateUpload, Attempt: 2, Fault: fakes3.ResetAt(0)})
	_, err = store.CreateMultipartUpload(ctx, ref)
	if !errors.As(err, &opErr) || opErr.Class != engine.ClassRetryable || opErr.Status != 0 {
		t.Fatalf("reset: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.CreateMultipartUpload(cancelled, ref)
	if !errors.As(err, &opErr) || opErr.Class != engine.ClassCancelled {
		t.Fatalf("cancelled: %v", err)
	}

	if err := store.AbortMultipartUpload(ctx, ref, "no-such-upload"); err != nil {
		t.Fatalf("abort of a missing upload: %v", err)
	}
}

type failingProvider struct{}

func (failingProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{}, errors.New("helper exited with status 1")
}

func TestCredentialProviderFailureIsClassified(t *testing.T) {
	server := newServer(t)
	client := server.S3Client(func(o *s3.Options) {
		o.Credentials = aws.NewCredentialsCache(s3x.MarkCredentialErrors(failingProvider{}))
	})
	_, err := s3x.NewStore(s3x.StaticClient(client)).CreateMultipartUpload(context.Background(), ref)
	if !errors.Is(err, engine.ErrCredentialsUnavailable) || engine.Classify(err) != engine.ClassCredential {
		t.Fatalf("got %v", err)
	}
	if n := len(server.Requests()); n != 0 {
		t.Fatalf("%d requests were sent without credentials", n)
	}
}

func TestGetObjectRangeChecks(t *testing.T) {
	server := newServer(t)
	store := newStore(server)
	ctx := context.Background()
	if err := server.PutSynthetic(fakes3.DefaultBucket, key, seed, 4*mib); err != nil {
		t.Fatal(err)
	}
	info, err := store.HeadObject(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	out, err := store.GetObjectRange(ctx, engine.GetRangeRequest{Ref: ref, Offset: mib, Length: 1024, IfMatch: info.ETag, Size: info.Size})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	got, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 1024)
	fakes3.Pattern{Seed: seed}.Fill(want, mib)
	if !bytes.Equal(got, want) {
		t.Fatal("ranged bytes differ from the pattern")
	}

	_, err = store.GetObjectRange(ctx, engine.GetRangeRequest{Ref: ref, Offset: 0, Length: 10, IfMatch: `"stale"`, Size: info.Size})
	if !errors.Is(err, engine.ErrObjectChanged) || engine.Classify(err) != engine.ClassPermanent {
		t.Fatalf("stale ETag: %v", err)
	}

	// The right ETag with a different total is a changed object, not a retry.
	_, err = store.GetObjectRange(ctx, engine.GetRangeRequest{Ref: ref, Offset: 0, Length: 10, IfMatch: info.ETag, Size: info.Size + 1})
	if !errors.Is(err, engine.ErrObjectChanged) || engine.Classify(err) != engine.ClassPermanent {
		t.Fatalf("wrong total: %v", err)
	}
	// A request without a size is rejected before it is sent.
	_, err = store.GetObjectRange(ctx, engine.GetRangeRequest{Ref: ref, Offset: 0, Length: 10, IfMatch: info.ETag})
	if !errors.Is(err, engine.ErrInvalidSize) {
		t.Fatalf("no size: %v", err)
	}
}

func TestTuneTransport(t *testing.T) {
	transport := &http.Transport{}
	s3x.TuneTransport(transport)
	if transport.MaxConnsPerHost != 0 || transport.MaxIdleConns != engine.MaxActivePartsLimit ||
		transport.MaxIdleConnsPerHost != engine.MaxActivePartsLimit || transport.ExpectContinueTimeout != time.Second ||
		transport.ReadBufferSize != 64*1024 || transport.WriteBufferSize != 64*1024 {
		t.Fatalf("%+v", transport)
	}
	if s3x.NewHTTPClient().GetTransport().MaxIdleConnsPerHost != engine.MaxActivePartsLimit {
		t.Fatal("NewHTTPClient does not use the tuned transport")
	}
}
