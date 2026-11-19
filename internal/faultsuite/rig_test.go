package faultsuite_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

const (
	mib      = int64(1) << 20
	runID    = "fault"
	seed     = uint64(0xfa17)
	partSize = 5 * mib
	objSize  = 80 * mib // 16 parts of partSize.
	parts    = 16
	profile  = "work"
	srcPath  = "source.bin"
)

func key(name string) string { return engine.TestKeyRoot + runID + "/" + name }

func refFor(k string) engine.ObjectRef {
	return engine.ObjectRef{Profile: profile, Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: k}
}

// newServer starts the HTTPS fake with the server-side guard. Its cleanup closes
// it, after which the test fails if any goroutine outlives the test.
func newServer(t *testing.T) (*fakes3.Server, *engine.TestPrefixGuard) {
	t.Helper()
	baseline := runtime.NumGoroutine()
	t.Cleanup(func() { assertNoLeak(t, baseline) })
	guard, err := engine.NewTestPrefixGuard(runID)
	if err != nil {
		t.Fatal(err)
	}
	server, err := fakes3.New(fakes3.Options{Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server, guard
}

func assertNoLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// endpointOptions points a client at the fake through the production transport
// (s3x.TuneTransport, including the 4 MiB send buffer), trusting the fake's
// certificate.
func endpointOptions(endpoint string, certPEM []byte) func(*s3.Options) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
		s3x.TuneTransport(tr)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	})
	return func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.HTTPClient = httpClient
	}
}

// fakeKeyClients serves every profile with the fake's own keys.
func fakeKeyClients(endpoint string, certPEM []byte) s3x.ClientFunc {
	static := credentials.NewStaticCredentialsProvider(fakes3.AccessKeyID, fakes3.SecretAccessKey, "")
	cache := aws.NewCredentialsCache(static)
	return s3x.NewClients(func(string) (aws.CredentialsProvider, error) { return cache, nil }, endpointOptions(endpoint, certPEM))
}

// passProber passes every probe.
type passProber struct{}

func (passProber) Probe(context.Context, engine.ObjectRef) error { return nil }

// gatedProber fails until open is closed.
type gatedProber struct{ open chan struct{} }

func newGatedProber() *gatedProber { return &gatedProber{open: make(chan struct{})} }

func (p *gatedProber) Probe(context.Context, engine.ObjectRef) error {
	select {
	case <-p.open:
		return nil
	default:
		return errors.New("credentials still expired")
	}
}

type rigOptions struct {
	clients  s3x.ClientFunc
	prober   engine.CredentialProber
	sink     engine.Sink
	settings func(*engine.Settings)
	retry    func(*engine.RetryPolicy)
	timeouts func(*engine.Timeouts)
}

// rig is one app session: a journal opened at launch, launch cleanup and an
// engine using the s3x adapter over HTTPS.
type rig struct {
	t        *testing.T
	server   *fakes3.Server
	guard    *engine.TestPrefixGuard
	dir      string
	journal  *journal.FileStore
	recovery *journal.Recovery
	source   *fakes3.Source
	sink     *fakes3.Sink
	eng      engine.Engine
	clients  s3x.ClientFunc
	closed   bool
}

// launch opens the journal in dir as the app does at start, creates launch
// cleanup and starts the engine. It does not run cleanup.
func launch(t *testing.T, server *fakes3.Server, guard *engine.TestPrefixGuard, dir string, o rigOptions) *rig {
	t.Helper()
	store := openJournal(t, dir)
	r := &rig{t: t, server: server, guard: guard, dir: dir, journal: store, source: fakes3.NewSource(), clients: o.clients}
	if r.clients == nil {
		r.clients = fakeKeyClients(server.Endpoint(), server.CertPEM())
	}
	var err error
	r.recovery, err = journal.NewRecovery(journal.RecoveryOptions{
		Store: store, Cleaner: fakes3.GuardCleaner(s3x.NewCleaner(r.clients), guard), BaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.source.Set(srcPath, fakes3.SyntheticFile{Size: objSize, Seed: seed})
	r.sink = fakes3.NewSink(func(string) uint64 { return seed })
	var sink engine.Sink = r.sink
	if o.sink != nil {
		sink = o.sink
	}
	prober := o.prober
	if prober == nil {
		prober = passProber{}
	}
	settings := engine.DefaultSettings()
	settings.PartSize = partSize
	if o.settings != nil {
		o.settings(&settings)
	}
	retry := engine.RetryPolicy{
		BaseDelay: 5 * time.Millisecond, ThrottleBaseDelay: 5 * time.Millisecond, MaxDelay: 50 * time.Millisecond,
		MaxPartAttempts: 10, MaxControlAttempts: 8, MaxAbortAttempts: 3, OutageTolerance: 0,
	}
	if o.retry != nil {
		o.retry(&retry)
	}
	timeouts := engine.DefaultTimeouts()
	timeouts.ProbeMin, timeouts.ProbeMax = 10*time.Millisecond, 50*time.Millisecond
	timeouts.ShutdownAbort = 5 * time.Second
	if o.timeouts != nil {
		o.timeouts(&timeouts)
	}
	r.eng, err = engine.New(engine.Config{
		Store: fakes3.GuardStore(s3x.NewStore(r.clients), guard), Source: r.source, Sink: sink,
		Journal: store, Prober: prober, Clock: engine.RealClock{}, Guard: guard,
		Jitter: func() float64 { return 0.5 }, Settings: settings, Retry: retry, Timeouts: timeouts,
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(r.shutdown)
	return r
}

// openJournal opens the journal, waiting for a killed process's lock to go.
func openJournal(t *testing.T, dir string) *journal.FileStore {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		store, err := journal.OpenFile(dir)
		if err == nil {
			return store
		}
		if !errors.Is(err, journal.ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("open journal: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shutdown closes the engine, then the journal, as the app does on exit.
func (r *rig) shutdown() {
	if r.closed {
		return
	}
	r.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.eng.Close(ctx); err != nil {
		r.t.Errorf("engine Close: %v", err)
	}
	if err := r.journal.Close(); err != nil {
		r.t.Errorf("journal Close: %v", err)
	}
}

func (r *rig) recover() journal.Report {
	r.t.Helper()
	report, err := r.recovery.Recover(context.Background())
	if err != nil {
		r.t.Fatalf("Recover: %v", err)
	}
	return report
}

func (r *rig) upload(k string) engine.JobID {
	r.t.Helper()
	id, err := r.eng.Submit(context.Background(), engine.JobSpec{Direction: engine.Upload, Ref: refFor(k), LocalPath: srcPath})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *rig) download(k, dest string) engine.JobID {
	r.t.Helper()
	id, err := r.eng.Submit(context.Background(), engine.JobSpec{Direction: engine.Download, Ref: refFor(k), LocalPath: dest})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *rig) snapshot(id engine.JobID) engine.JobSnapshot {
	r.t.Helper()
	all, err := r.eng.Snapshot(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	for _, job := range all {
		if job.ID == id {
			return job
		}
	}
	r.t.Fatalf("job %s not found", id)
	return engine.JobSnapshot{}
}

func (r *rig) waitFor(id engine.JobID, what string, ok func(engine.JobSnapshot) bool) engine.JobSnapshot {
	r.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		job := r.snapshot(id)
		if ok(job) {
			return job
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s: %+v", what, job)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *rig) waitState(id engine.JobID, state engine.JobState) engine.JobSnapshot {
	r.t.Helper()
	return r.waitFor(id, state.String(), func(job engine.JobSnapshot) bool {
		if job.State.IsTerminal() && job.State != state {
			r.t.Fatalf("job ended %s, want %s: %+v %q", job.State, state, job.Err, job.Warning)
		}
		return job.State == state
	})
}

// assertNoUploads proves S3 holds no multipart upload.
func assertNoUploads(t *testing.T, server *fakes3.Server) {
	t.Helper()
	if uploads := server.ActiveUploads(fakes3.DefaultBucket); len(uploads) != 0 {
		t.Fatalf("multipart uploads remain: %+v", uploads)
	}
}

// assertClean proves no multipart upload and no journal entry remain.
func (r *rig) assertClean() {
	r.t.Helper()
	assertNoUploads(r.t, r.server)
	if entries := r.journal.List(); len(entries) != 0 {
		r.t.Fatalf("journal entries remain: %+v", entries)
	}
}

func assertVerified(t *testing.T, server *fakes3.Server, k string) {
	t.Helper()
	object, ok := server.Object(fakes3.DefaultBucket, k)
	if !ok {
		t.Fatalf("object %s does not exist", k)
	}
	if object.Size != objSize {
		t.Fatalf("object is %d bytes, want %d", object.Size, objSize)
	}
	if err := server.VerifyObject(fakes3.DefaultBucket, k, seed, 1); err != nil {
		t.Fatal(err)
	}
}

// assertEachPartStoredOnce proves no finished part was sent again: every part
// number has exactly one successful UploadPart.
func assertEachPartStoredOnce(t *testing.T, server *fakes3.Server, k string) {
	t.Helper()
	ok := make(map[int32]int)
	for _, rec := range server.Requests() {
		if rec.Op == fakes3.OpUploadPart && rec.Key == k && rec.Status == http.StatusOK {
			ok[rec.Part]++
		}
	}
	for part := int32(1); part <= parts; part++ {
		if ok[part] != 1 {
			t.Errorf("part %d was stored %d times", part, ok[part])
		}
	}
}

// stallParts holds every part from first on, so a transfer cannot finish.
func stallParts(server *fakes3.Server, k string, first int32) {
	for part := first; part <= parts; part++ {
		server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: k, Part: part, Fault: fakes3.StallAt(1, 0)})
	}
}

// stallRanges holds every ranged GET of a download from part first on.
func stallRanges(server *fakes3.Server, k string, first int32) {
	for part := first; part <= parts; part++ {
		offset := int64(part-1) * partSize
		server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: k, RangeStart: &offset, Fault: fakes3.StallAt(1, 0)})
	}
}
