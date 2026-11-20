package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/creds"
	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/fsx"
	"s4browser/internal/journal"
	"s4browser/internal/queue"
)

// These tests drive the queue only through App's binding methods, against the
// HTTPS fake S3, with a real profile manager over temporary AWS files holding
// fake keys. Nothing reads ~/.aws or %AppData%\s4browser.

const (
	tMiB    = int64(1) << 20
	tRun    = "queue"
	tSeed   = uint64(0x51)
	tPart   = 5 * tMiB
	tSize   = 80 * tMiB // 16 parts of tPart.
	tParts  = 16
	tName   = "work"
	oldKey  = "ASIAFAKEOLDSESSION01"
	newKey  = "ASIAFAKENEWSESSION02"
	tPrefix = engine.TestKeyRoot + tRun + "/"
)

var tProfile = creds.AWSID(tName)

// memVault is an in-memory creds.Vault. These tests use only AWS profiles.
type memVault struct {
	mu sync.Mutex
	m  map[string]string
}

func (v *memVault) Set(user, secret string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[user] = secret
	return nil
}

func (v *memVault) Get(user string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.m[user]; ok {
		return s, nil
	}
	return "", creds.ErrSecretNotFound
}

func (v *memVault) Delete(user string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.m, user)
	return nil
}

// eventLog records what the app would send to the window.
type eventLog struct {
	mu     sync.Mutex
	queue  []queue.Event
	status []queue.Status
	times  []time.Time
}

func (e *eventLog) emit(name string, payload any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch p := payload.(type) {
	case queue.Event:
		e.queue = append(e.queue, p)
		e.times = append(e.times, time.Now())
	case queue.Status:
		e.status = append(e.status, p)
	default:
		panic("unexpected event " + name)
	}
}

func (e *eventLog) queueEvents() []queue.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]queue.Event(nil), e.queue...)
}

func (e *eventLog) statusCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.status)
}

// rig is one app session.
type rig struct {
	t       *testing.T
	server  *fakes3.Server
	guard   *engine.TestPrefixGuard
	app     *App
	ev      *eventLog
	dir     string // The queue's folder: journal and settings.
	files   string // Local files.
	profile *awsProfile
}

type rigOptions struct {
	dir      string // Reuse a queue folder, as a later launch does.
	key      string // Access key of the profile. Default the fake's own.
	session  bool   // The profile has a session token.
	timeouts func(*engine.Timeouts)
	// keepSettings leaves the transfer settings as the launch found them.
	keepSettings bool
	// beforeLaunch runs after the fake and the profile exist and before the queue starts.
	beforeLaunch func(*rig)
}

// awsProfile is a profile in temporary AWS files.
type awsProfile struct {
	t           *testing.T
	credentials string
	manager     *creds.Manager
}

func newAWSProfile(t *testing.T, key string, session bool) *awsProfile {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("[profile "+tName+"]\nregion = "+fakes3.Region+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &awsProfile{t: t, credentials: filepath.Join(dir, "credentials")}
	p.signIn(key, session)
	var err error
	p.manager, err = creds.Open(creds.Options{
		Dir: filepath.Join(dir, "state"), Vault: &memVault{m: map[string]string{}},
		AWSConfigFile: config, AWSCredentialsFile: p.credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *awsProfile) signIn(key string, session bool) {
	p.t.Helper()
	text := "[" + tName + "]\naws_access_key_id = " + key + "\naws_secret_access_key = fake-secret-" + key + "\n"
	if session {
		text += "aws_session_token = fake-token-" + key + "\n"
	}
	if err := os.WriteFile(p.credentials, []byte(text), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// newRig starts the fake and one app session.
func newRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	baseline := runtime.NumGoroutine()
	t.Cleanup(func() { assertNoLeak(t, baseline) })
	guard, err := engine.NewTestPrefixGuard(tRun)
	if err != nil {
		t.Fatal(err)
	}
	server, err := fakes3.New(fakes3.Options{Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	key := o.key
	if key == "" {
		key = fakes3.AccessKeyID
	}
	r := &rig{t: t, server: server, guard: guard, ev: &eventLog{}, files: t.TempDir(), profile: newAWSProfile(t, key, o.session)}
	r.launch(o)
	return r
}

// launch starts an app session, which closes with the test.
func (r *rig) launch(o rigOptions) {
	r.t.Helper()
	r.dir = o.dir
	if r.dir == "" {
		r.dir = r.t.TempDir()
	}
	r.app = &App{ctx: context.Background(), profiles: r.profile.manager}
	qo := r.queueOptions(r.dir, o)
	var err error
	if r.app.queue, err = queue.Open(qo); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(r.app.closeQueue)
	if !o.keepSettings {
		if _, err := r.app.SetTransferSettings(queue.TransferSettings{ParallelFiles: 4, PartsPerFile: 4, TotalParts: 8, PartSize: tPart}); err != nil {
			r.t.Fatal(err)
		}
	}
}

// queueOptions is App.queueOptions with the fake S3 behind it.
func (r *rig) queueOptions(dir string, o rigOptions) queue.Options {
	qo := r.app.queueOptions(dir)
	qo.Emit = r.ev.emit
	qo.ClientOptions = []func(*s3.Options){func(so *s3.Options) {
		so.BaseEndpoint = aws.String(r.server.Endpoint())
		so.UsePathStyle = true
		so.HTTPClient = r.server.HTTPClient()
	}}
	qo.Guard = r.guard
	qo.WrapStore = func(s engine.ObjectStore) engine.ObjectStore { return fakes3.GuardStore(s, r.guard) }
	qo.WrapCleaner = func(c journal.RemoteCleaner) journal.RemoteCleaner { return fakes3.GuardCleaner(c, r.guard) }
	qo.RecoveryBaseDelay = time.Millisecond
	retry := engine.RetryPolicy{
		BaseDelay: 5 * time.Millisecond, ThrottleBaseDelay: 5 * time.Millisecond, MaxDelay: 50 * time.Millisecond,
		MaxPartAttempts: 10, MaxControlAttempts: 8, MaxAbortAttempts: 3,
	}
	qo.Retry = &retry
	timeouts := engine.DefaultTimeouts()
	timeouts.ProbeMin, timeouts.ProbeMax = 10*time.Millisecond, 50*time.Millisecond
	timeouts.ShutdownAbort = 5 * time.Second
	if o.timeouts != nil {
		o.timeouts(&timeouts)
	}
	qo.Timeouts = &timeouts
	return qo
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

// pattern writes a local file of the pattern for seed.
func (r *rig) pattern(name string, seed uint64, size int64) string {
	r.t.Helper()
	path := filepath.Join(r.files, name)
	f, err := os.Create(path)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for off := int64(0); off < size; off += int64(len(buf)) {
		chunk := buf[:min(int64(len(buf)), size-off)]
		fakes3.Pattern{Seed: seed}.Fill(chunk, off)
		if _, err := f.Write(chunk); err != nil {
			r.t.Fatal(err)
		}
	}
	return path
}

func (r *rig) upload(paths ...string) []string {
	r.t.Helper()
	res, err := r.app.QueueUploads(queue.UploadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Prefix: tPrefix, Paths: paths})
	if err != nil {
		r.t.Fatal(err)
	}
	ids := make([]string, len(res.Items))
	for i, it := range res.Items {
		if it.JobID == "" {
			r.t.Fatalf("%s was not queued: %s", it.Source, it.Error)
		}
		ids[i] = it.JobID
	}
	return ids
}

func (r *rig) download(dest string, keys ...string) []string {
	r.t.Helper()
	res, err := r.app.QueueDownloads(queue.DownloadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Keys: keys, DestDir: dest})
	if err != nil {
		r.t.Fatal(err)
	}
	ids := make([]string, len(res.Items))
	for i, it := range res.Items {
		if it.JobID == "" {
			r.t.Fatalf("%s was not queued: %s", it.Source, it.Error)
		}
		ids[i] = it.JobID
	}
	return ids
}

func (r *rig) job(id string) queue.JobView {
	r.t.Helper()
	jobs, err := r.app.QueueJobs()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	r.t.Fatalf("job %s is not in the list", id)
	return queue.JobView{}
}

// wait polls the job list through the binding until pred holds.
func (r *rig) wait(id, what string, pred func(queue.JobView) bool) queue.JobView {
	r.t.Helper()
	var last queue.JobView
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := r.app.QueueJobs()
		if err != nil {
			r.t.Fatal(err)
		}
		for _, j := range jobs {
			if j.ID == id {
				last = j
				if pred(j) {
					return j
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s; last state: %+v", what, last)
	return last
}

func (r *rig) waitState(id, state string) queue.JobView {
	r.t.Helper()
	return r.wait(id, state, func(j queue.JobView) bool { return j.State == state })
}

func (r *rig) slowParts(key string, d time.Duration) {
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: key, Fault: fakes3.SlowResponse(d)})
}

func (r *rig) assertVerified(key string, size int64, seed uint64) {
	r.t.Helper()
	obj, ok := r.server.Object(fakes3.DefaultBucket, key)
	if !ok || obj.Size != size {
		r.t.Fatalf("object %s: exists %v, %+v", key, ok, obj)
	}
	if err := r.server.VerifyObject(fakes3.DefaultBucket, key, seed, 1); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) assertNoUploads() {
	r.t.Helper()
	if u := r.server.ActiveUploads(fakes3.DefaultBucket); len(u) != 0 {
		r.t.Fatalf("multipart uploads left on S3: %+v", u)
	}
}

// assertFile checks that a local file holds the pattern for seed.
func assertFile(t *testing.T, path string, seed uint64, size int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf, scratch := make([]byte, 1<<20), make([]byte, 1<<20)
	var off int64
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			if at, ok := (fakes3.Pattern{Seed: seed}).Equal(buf[:n], off, scratch); !ok {
				t.Fatalf("%s differs at byte %d", path, at)
			}
			off += int64(n)
		}
		if err != nil {
			break
		}
	}
	if off != size {
		t.Fatalf("%s is %d bytes, want %d", path, off, size)
	}
}

func uploadPartRequests(server *fakes3.Server, key string) int {
	n := 0
	for _, rec := range server.Requests() {
		if rec.Op == fakes3.OpUploadPart && rec.Key == key {
			n++
		}
	}
	return n
}

func journalEntries(t *testing.T, dir string) []journal.Entry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, journal.FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	entries, err := journal.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestUploadAndDownloadRoundTrip: an upload and a download through the bindings,
// byte for byte, then the finished jobs are cleared.
func TestUploadAndDownloadRoundTrip(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := r.pattern("clip.bin", tSeed, tSize)
	key := tPrefix + "clip.bin"

	up := r.upload(src)[0]
	job := r.waitState(up, "completed")
	if job.Direction != "upload" || job.Bucket != fakes3.DefaultBucket || job.Key != key || job.Name != "clip.bin" || job.Profile != tProfile {
		t.Fatalf("job identity: %+v", job)
	}
	if job.TotalBytes != tSize || job.DoneBytes != tSize || job.PartsTotal != tParts || job.PartsDone != tParts || !job.Finished || job.CanPause || !job.CanRemove {
		t.Fatalf("finished upload: %+v", job)
	}
	if job.Reason != "" || job.Warning != "" {
		t.Fatalf("a completed job has a reason: %+v", job)
	}
	r.assertVerified(key, tSize, tSeed)

	dest := t.TempDir()
	down := r.download(dest, key)[0]
	job = r.waitState(down, "completed")
	if job.Direction != "download" || job.LocalPath != filepath.Join(dest, "clip.bin") || job.DoneBytes != tSize {
		t.Fatalf("finished download: %+v", job)
	}
	assertFile(t, filepath.Join(dest, "clip.bin"), tSeed, tSize)
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Fatalf("the download folder holds %d entries, want only the file", len(entries))
	}
	if left := journalEntries(t, r.dir); len(left) != 0 {
		t.Fatalf("journal entries after completion: %+v", left)
	}

	// Clearing drops both, and the UI is told which.
	if n, err := r.app.ClearFinished(false); err != nil || n != 2 {
		t.Fatalf("ClearFinished = %d, %v", n, err)
	}
	if jobs, _ := r.app.QueueJobs(); len(jobs) != 0 {
		t.Fatalf("jobs after clearing: %+v", jobs)
	}
	removed := map[string]bool{}
	for _, ev := range r.ev.queueEvents() {
		for _, id := range ev.Removed {
			removed[id] = true
		}
	}
	if !removed[up] || !removed[down] || len(removed) != 2 {
		t.Fatalf("removed events: %v", removed)
	}
	if n, _ := r.app.ClearFinished(false); n != 0 {
		t.Fatalf("clearing again removed %d", n)
	}
}

// TestPauseAndResume: a paused job sends nothing, and resumes where it stopped.
func TestPauseAndResume(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := r.pattern("clip.bin", tSeed, tSize)
	key := tPrefix + "clip.bin"
	r.slowParts(key, 120*time.Millisecond)
	if _, err := r.app.SetTransferSettings(queue.TransferSettings{ParallelFiles: 4, PartsPerFile: 2, TotalParts: 8, PartSize: tPart}); err != nil {
		t.Fatal(err)
	}

	id := r.upload(src)[0]
	r.wait(id, "three finished parts", func(j queue.JobView) bool { return j.State == "running" && j.PartsDone >= 3 })
	if err := r.app.PauseJob(id); err != nil {
		t.Fatal(err)
	}
	job := r.waitState(id, "paused")
	if !job.CanResume || job.CanPause || !job.CanCancel || job.Reason == "" || job.PartsDone < 3 || job.PartsDone >= tParts {
		t.Fatalf("paused job: %+v", job)
	}
	// Requests the pause cancelled are still being logged by the server.
	time.Sleep(300 * time.Millisecond)
	sent := uploadPartRequests(r.server, key)
	time.Sleep(500 * time.Millisecond)
	if now := uploadPartRequests(r.server, key); now != sent {
		t.Fatalf("a paused upload sent %d more part requests", now-sent)
	}
	if err := r.app.PauseJob(id); err == nil {
		t.Fatal("pausing a paused job succeeded")
	}

	if err := r.app.ResumeJob(id); err != nil {
		t.Fatal(err)
	}
	job = r.waitState(id, "completed")
	if job.PartsDone != tParts {
		t.Fatalf("resumed job: %+v", job)
	}
	r.assertVerified(key, tSize, tSeed)
	// Resuming continued from the finished parts: only parts that were in flight
	// when the pause cancelled them may be sent twice (two at a time here).
	if total := uploadPartRequests(r.server, key); total > tParts+2 {
		t.Fatalf("%d part requests for %d parts: finished parts were sent again", total, tParts)
	}
	if err := r.app.ResumeJob(id); err == nil {
		t.Fatal("resuming a completed job succeeded")
	}
}

// TestCancelLeavesNoMultipartUpload: cancel aborts the upload on S3 and clears
// the journal.
func TestCancelLeavesNoMultipartUpload(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := r.pattern("clip.bin", tSeed, tSize)
	key := tPrefix + "clip.bin"
	r.slowParts(key, 100*time.Millisecond)

	id := r.upload(src)[0]
	r.wait(id, "an upload to exist on S3", func(j queue.JobView) bool { return j.PartsDone >= 2 })
	if u := r.server.ActiveUploads(fakes3.DefaultBucket); len(u) != 1 {
		t.Fatalf("uploads on S3 while running: %+v", u)
	}
	if len(journalEntries(t, r.dir)) == 0 {
		t.Fatal("a running upload is not in the journal")
	}
	if err := r.app.CancelJob(id); err != nil {
		t.Fatal(err)
	}
	job := r.waitState(id, "cancelled")
	if !job.Finished || !job.CanRemove || job.CanCancel || job.Warning != "" {
		t.Fatalf("cancelled job: %+v", job)
	}
	r.assertNoUploads()
	if _, ok := r.server.Object(fakes3.DefaultBucket, key); ok {
		t.Fatal("a cancelled upload created an object")
	}
	if left := journalEntries(t, r.dir); len(left) != 0 {
		t.Fatalf("journal entries after cancel: %+v", left)
	}
	if err := r.app.CancelJob(id); err == nil {
		t.Fatal("cancelling a cancelled job succeeded")
	}
}

// TestBulkCommands: pause, resume and cancel for every job at once.
func TestBulkCommands(t *testing.T) {
	r := newRig(t, rigOptions{})
	var paths []string
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		paths = append(paths, r.pattern(name, tSeed, tSize))
		r.slowParts(tPrefix+name, 100*time.Millisecond)
	}
	if _, err := r.app.SetTransferSettings(queue.TransferSettings{ParallelFiles: 1, PartsPerFile: 2, TotalParts: 8, PartSize: tPart}); err != nil {
		t.Fatal(err)
	}
	ids := r.upload(paths...)
	r.wait(ids[0], "the first job to run", func(j queue.JobView) bool { return j.PartsDone >= 1 })
	if j := r.job(ids[2]); j.State != "queued" || j.Reason == "" {
		t.Fatalf("with one slot the third job waits: %+v", j)
	}

	if n, err := r.app.PauseAll(); err != nil || n != 3 {
		t.Fatalf("PauseAll = %d, %v", n, err)
	}
	for _, id := range ids {
		r.waitState(id, "paused")
	}
	if n, _ := r.app.PauseAll(); n != 0 {
		t.Fatalf("PauseAll on paused jobs = %d", n)
	}
	if n, err := r.app.ResumeAll(); err != nil || n != 3 {
		t.Fatalf("ResumeAll = %d, %v", n, err)
	}
	r.wait(ids[0], "the first job running again", func(j queue.JobView) bool { return j.State == "running" })
	if n, err := r.app.CancelAll(); err != nil || n != 3 {
		t.Fatalf("CancelAll = %d, %v", n, err)
	}
	for _, id := range ids {
		r.waitState(id, "cancelled")
	}
	r.assertNoUploads()
	if n, err := r.app.ClearFinished(false); err != nil || n != 3 {
		t.Fatalf("ClearFinished = %d, %v", n, err)
	}
}

// TestFailedJobShowsItsReason: a failure carries a message, stays in the list
// until cleared on request, and can be retried.
func TestFailedJobShowsItsReason(t *testing.T) {
	r := newRig(t, rigOptions{})
	id := r.download(t.TempDir(), tPrefix+"missing.bin")[0]
	job := r.waitState(id, "failed")
	if job.Reason != "The object was not found in the bucket." || job.ErrorCode != "NotFound" || job.Detail == "" || !job.CanRetry || !job.CanRemove || !job.Finished {
		t.Fatalf("failed job: %+v", job)
	}
	if n, _ := r.app.ClearFinished(false); n != 0 {
		t.Fatalf("ClearFinished(false) removed a failed job")
	}
	if err := r.app.RetryJob(id); err != nil {
		t.Fatal(err)
	}
	r.waitState(id, "failed")
	if err := r.app.RemoveJob(id); err != nil {
		t.Fatal(err)
	}
	if err := r.app.RemoveJob(id); err == nil {
		t.Fatal("removing a job twice succeeded")
	}
	id = r.download(t.TempDir(), tPrefix+"missing.bin")[0]
	r.waitState(id, "failed")
	if n, _ := r.app.ClearFinished(true); n != 1 {
		t.Fatalf("ClearFinished(true) = %d", n)
	}
}

// TestSettingsChangeWhileRunning: a bandwidth cap applies to the running job at
// once, invalid settings are refused, and the file keeps what was set.
func TestSettingsChangeWhileRunning(t *testing.T) {
	r := newRig(t, rigOptions{keepSettings: true})
	state, err := r.app.TransferSettings()
	if err != nil {
		t.Fatal(err)
	}
	def := engine.DefaultSettings()
	if state.Current.ParallelFiles != def.MaxActiveFiles || state.Current.PartsPerFile != def.MaxPartsPerFile ||
		state.Current.TotalParts != def.MaxActiveParts || state.Current.PartSize != def.PartSize ||
		state.Current.UploadBytesPerSec != 0 || state.Current.DownloadBytesPerSec != 0 || state.Defaults != state.Current {
		t.Fatalf("defaults: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(r.dir, queue.SettingsFileName)); err == nil {
		t.Fatal("the settings file exists before anything was changed")
	}

	bad := state.Current
	bad.PartsPerFile = 0
	if _, err := r.app.SetTransferSettings(bad); err == nil {
		t.Fatal("invalid settings were accepted")
	}
	if _, err := os.Stat(filepath.Join(r.dir, queue.SettingsFileName)); err == nil {
		t.Fatal("invalid settings were saved")
	}

	capped := queue.TransferSettings{ParallelFiles: 2, PartsPerFile: 3, TotalParts: 6, PartSize: tPart, UploadBytesPerSec: 2 * tMiB}
	if got, err := r.app.SetTransferSettings(capped); err != nil || got.Current != capped {
		t.Fatalf("SetTransferSettings returned %+v, %v", got.Current, err)
	}
	if got, _ := r.app.TransferSettings(); got.Current != capped {
		t.Fatalf("TransferSettings after a change: %+v", got.Current)
	}
	src := r.pattern("clip.bin", tSeed, tSize)
	id := r.upload(src)[0]
	time.Sleep(1500 * time.Millisecond)
	job := r.job(id)
	if job.State != "running" || job.DoneBytes >= 25*tMiB {
		t.Fatalf("a 2 MiB/s cap let %d bytes through in 1.5 s: %+v", job.DoneBytes, job)
	}

	started := time.Now()
	capped.UploadBytesPerSec = 0
	if _, err := r.app.SetTransferSettings(capped); err != nil {
		t.Fatal(err)
	}
	r.waitState(id, "completed")
	if took := time.Since(started); took > 15*time.Second {
		t.Fatalf("raising the cap did not speed up the running job: %v", took)
	}
	r.assertVerified(tPrefix+"clip.bin", tSize, tSeed)

	// The file holds the last values, and the next launch reads them.
	var onDisk map[string]any
	data, err := os.ReadFile(filepath.Join(r.dir, queue.SettingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk["parallelFiles"] != float64(2) || onDisk["partsPerFile"] != float64(3) || onDisk["uploadBytesPerSec"] != float64(0) {
		t.Fatalf("settings file: %s (%v)", data, err)
	}
	r.app.closeQueue()
	r.launch(rigOptions{dir: r.dir, keepSettings: true})
	if got, _ := r.app.TransferSettings(); got.Current != capped {
		t.Fatalf("settings after relaunch: %+v, want %+v", got.Current, capped)
	}
}

// TestDamagedSettingsFileIsKeptAside: the defaults apply and the user is told.
func TestDamagedSettingsFileIsKeptAside(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, queue.SettingsFileName), []byte(`{"version":1,"parallelFiles":999`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, rigOptions{dir: dir, keepSettings: true})
	state, _ := r.app.TransferSettings()
	if state.Current != state.Defaults {
		t.Fatalf("settings: %+v", state.Current)
	}
	st := r.app.TransferStatus()
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "transfer settings") {
		t.Fatalf("warnings: %v", st.Warnings)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, queue.SettingsFileName+".bad-*"))
	if len(kept) != 1 {
		t.Fatalf("damaged file kept aside: %v", kept)
	}
}

// TestCredentialWaitThenRecovery: expired credentials put the job in a visible
// credential wait with a reason. Signing in and loading the credentials again,
// which is the CheckProfile binding, resumes it at once, with the probes an hour
// apart, and no finished part is sent again.
func TestCredentialWaitThenRecovery(t *testing.T) {
	r := newRig(t, rigOptions{
		key: oldKey, session: true,
		timeouts: func(to *engine.Timeouts) { to.ProbeMin, to.ProbeMax = time.Hour, time.Hour },
	})
	src := r.pattern("clip.bin", tSeed, tSize)
	key := tPrefix + "clip.bin"
	r.slowParts(key, 60*time.Millisecond)

	id := r.upload(src)[0]
	r.wait(id, "three finished parts", func(j queue.JobView) bool { return j.PartsDone >= 3 })
	r.server.AddRule(fakes3.Rule{AccessKey: oldKey, Fault: fakes3.ExpiredToken()})
	job := r.waitState(id, "credential-wait")
	if !strings.Contains(job.Reason, tProfile) || !strings.Contains(job.Reason, "credentials") || !job.CanPause || !job.CanCancel || job.Finished {
		t.Fatalf("credential wait: %+v", job)
	}
	if job.Retries != 0 {
		t.Fatalf("credential failures were charged as retries: %+v", job)
	}
	done := job.PartsDone
	time.Sleep(300 * time.Millisecond)
	if job = r.job(id); job.State != "credential-wait" {
		t.Fatalf("the job left the wait with the credentials unchanged: %+v", job)
	}

	// The event stream shows the wait too.
	sawWait := false
	for _, ev := range r.ev.queueEvents() {
		for _, j := range ev.Jobs {
			sawWait = sawWait || (j.ID == id && j.State == "credential-wait")
		}
	}
	if !sawWait {
		t.Fatal("no event carried the credential wait")
	}

	r.profile.signIn(newKey, true)
	if check := r.app.CheckProfile(tProfile); !check.OK {
		t.Fatalf("CheckProfile: %+v", check)
	}
	job = r.waitState(id, "completed")
	if job.Retries != 0 || job.PartsDone != tParts || done < 3 {
		t.Fatalf("recovered job: %+v", job)
	}
	r.assertVerified(key, tSize, tSeed)
}

// TestEventsAreBatched: many parts finish, and the window receives about five
// events a second, in sequence, never one per part.
func TestEventsAreBatched(t *testing.T) {
	r := newRig(t, rigOptions{})
	const size = 100 * tMiB // 20 parts
	src := r.pattern("clip.bin", tSeed, size)
	key := tPrefix + "clip.bin"
	r.slowParts(key, 150*time.Millisecond)
	if _, err := r.app.SetTransferSettings(queue.TransferSettings{ParallelFiles: 1, PartsPerFile: 2, TotalParts: 2, PartSize: tPart}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	id := r.upload(src)[0]
	r.waitState(id, "completed")
	// The last batch is delivered a tick after the state changes.
	time.Sleep(450 * time.Millisecond)
	elapsed := time.Since(started)

	events := r.ev.queueEvents()
	if parts := uploadPartRequests(r.server, key); parts < 20 {
		t.Fatalf("only %d part requests", parts)
	}
	maxEvents := int(elapsed/(200*time.Millisecond)) + 2
	if len(events) < 3 || len(events) > maxEvents {
		t.Fatalf("%d events in %v for 20 parts, want 3 to %d", len(events), elapsed, maxEvents)
	}
	var progress bool
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d", i, ev.Seq)
		}
		if len(ev.Jobs) == 0 {
			t.Fatalf("empty event %d", i)
		}
		for _, j := range ev.Jobs {
			progress = progress || (j.State == "running" && j.DoneBytes > 0 && j.DoneBytes < j.TotalBytes)
		}
	}
	if !progress {
		t.Fatal("no event showed progress in the middle of the transfer")
	}
	last := events[len(events)-1].Jobs
	if last[len(last)-1].State != "completed" {
		t.Fatalf("the last event: %+v", last)
	}
	// The window never gets two events closer than a tick.
	r.ev.mu.Lock()
	defer r.ev.mu.Unlock()
	for i := 1; i < len(r.ev.times); i++ {
		if gap := r.ev.times[i].Sub(r.ev.times[i-1]); gap < 100*time.Millisecond {
			t.Fatalf("events %d and %d were %v apart", i-1, i, gap)
		}
	}
}

// TestLaunchCleanupRunsAndReportsWarnings: the previous session's upload is
// aborted in the background, the journal's warnings reach the window, and a
// journal entry that names a file that is not a temp file never deletes it.
func TestLaunchCleanupRunsAndReportsWarnings(t *testing.T) {
	dir := t.TempDir()
	r := newRigWithPriorSession(t, dir, rigOptions{})
	st := r.waitCleanup()
	if st.Cleanup.UploadsAborted != 1 || st.Cleanup.Pending != 0 || st.Cleanup.Error != "" {
		t.Fatalf("cleanup: %+v", st.Cleanup)
	}
	r.assertNoUploads()
	if left := journalEntries(t, dir); len(left) != 0 {
		t.Fatalf("journal after cleanup: %+v", left)
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "precious.mp4") || !strings.Contains(st.Warnings[0], "left alone") {
		t.Fatalf("warnings: %v", st.Warnings)
	}
	if _, err := os.Stat(filepath.Join(r.files, "precious.mp4")); err != nil {
		t.Fatalf("launch cleanup deleted a file that is not a temp file: %v", err)
	}
	if r.ev.statusCount() == 0 {
		t.Fatal("no s4:queue-status event reported the cleanup")
	}
	// New work is not held up by it.
	id := r.upload(r.pattern("after.bin", tSeed, 8*tMiB))[0]
	r.waitState(id, "completed")
}

// TestLaunchCleanupResumesWhenCredentialsChange: with the session expired, the
// leftover upload cannot be aborted. It stays in the journal and is aborted as
// soon as the credentials are loaded again, without another launch.
func TestLaunchCleanupResumesWhenCredentialsChange(t *testing.T) {
	r := newRigWithPriorSession(t, t.TempDir(), rigOptions{
		key: oldKey, session: true,
		beforeLaunch: func(r *rig) { r.server.AddRule(fakes3.Rule{AccessKey: oldKey, Fault: fakes3.ExpiredToken()}) },
	})
	st := r.waitCleanup()
	if st.Cleanup.UploadsAborted != 0 || st.Cleanup.Pending != 1 {
		t.Fatalf("cleanup with expired credentials: %+v", st.Cleanup)
	}
	if u := r.server.ActiveUploads(fakes3.DefaultBucket); len(u) != 1 {
		t.Fatalf("uploads on S3: %+v", u)
	}

	r.profile.signIn(newKey, true)
	if check := r.app.CheckProfile(tProfile); !check.OK {
		t.Fatalf("CheckProfile: %+v", check)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st = r.app.TransferStatus()
		if !st.Cleanup.Running && st.Cleanup.Pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup did not resume: %+v", st.Cleanup)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Cleanup.UploadsAborted != 1 {
		t.Fatalf("cleanup: %+v", st.Cleanup)
	}
	r.assertNoUploads()
}

// newRigWithPriorSession starts an app whose journal holds the leftovers of a
// crashed session: an upload with a recorded ID and a path that is not a temp file.
func newRigWithPriorSession(t *testing.T, dir string, o rigOptions) *rig {
	t.Helper()
	baseline := runtime.NumGoroutine()
	t.Cleanup(func() { assertNoLeak(t, baseline) })
	guard, _ := engine.NewTestPrefixGuard(tRun)
	server, err := fakes3.New(fakes3.Options{Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	key := o.key
	if key == "" {
		key = fakes3.AccessKeyID
	}
	r := &rig{t: t, server: server, guard: guard, ev: &eventLog{}, files: t.TempDir(), profile: newAWSProfile(t, key, o.session)}

	ref := engine.ObjectRef{Profile: tProfile, Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: tPrefix + "orphan.bin"}
	uploadID, err := server.Store().CreateMultipartUpload(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(r.files, "precious.mp4")
	if err := os.WriteFile(precious, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	prior, err := journal.OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, e := range []journal.Entry{
		{ID: "0123456789abcdef-1", Kind: journal.KindMultipartUpload, Phase: journal.PhaseActive, CreatedAt: now,
			Profile: ref.Profile, Region: ref.Region, Bucket: ref.Bucket, Key: ref.Key, UploadID: uploadID},
		{ID: "fedcba9876543210-1", Kind: journal.KindDownloadTemp, Phase: journal.PhaseActive, CreatedAt: now, TempPath: precious},
	} {
		if err := prior.Put(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	if o.beforeLaunch != nil {
		o.beforeLaunch(r)
	}
	o.dir = dir
	r.launch(o)
	return r
}

func (r *rig) waitCleanup() queue.Status {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st := r.app.TransferStatus(); st.Cleanup.Finished && !st.Cleanup.Running {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("launch cleanup did not finish: %+v", r.app.TransferStatus())
	return queue.Status{}
}

// TestCorruptJournalIsReported: the user is told, and transfers still work.
func TestCorruptJournalIsReported(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, journal.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, rigOptions{dir: dir})
	st := r.waitCleanup()
	if !st.Available || st.UploadsDisabled || len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "corrupt") {
		t.Fatalf("status: %+v", st)
	}
	r.waitState(r.upload(r.pattern("a.bin", tSeed, 8*tMiB))[0], "completed")
}

// TestUnsupportedJournalDisablesUploads: a journal from a newer build is left
// alone, uploads are refused with a reason, and downloads still work.
func TestUnsupportedJournalDisablesUploads(t *testing.T) {
	dir := t.TempDir()
	future := []byte(`{"version":99,"entries":[]}`)
	path := filepath.Join(dir, journal.FileName)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, rigOptions{dir: dir})
	st := r.app.TransferStatus()
	if !st.Available || !st.UploadsDisabled || !strings.Contains(st.UploadsDisabledReason, "newer version") {
		t.Fatalf("status: %+v", st)
	}
	_, err := r.app.QueueUploads(queue.UploadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Prefix: tPrefix, Paths: []string{r.pattern("a.bin", tSeed, tMiB)}})
	if err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("upload error: %v", err)
	}
	if jobs, _ := r.app.QueueJobs(); len(jobs) != 0 {
		t.Fatalf("a refused upload made a job: %+v", jobs)
	}

	// Downloads work, and the future journal is untouched.
	if err := r.server.PutSynthetic(fakes3.DefaultBucket, tPrefix+"d.bin", 7, 3*tMiB); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	r.waitState(r.download(dest, tPrefix+"d.bin")[0], "completed")
	assertFile(t, filepath.Join(dest, "d.bin"), 7, 3*tMiB)
	r.app.closeQueue()
	if got, err := os.ReadFile(path); err != nil || string(got) != string(future) {
		t.Fatalf("the unsupported journal was changed: %q, %v", got, err)
	}
}

// TestSecondInstanceIsRefused: a second app cannot start transfers, says why,
// and leaves the first one's journal alone.
func TestSecondInstanceIsRefused(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := r.pattern("clip.bin", tSeed, tSize)
	key := tPrefix + "clip.bin"
	r.slowParts(key, 80*time.Millisecond)
	id := r.upload(src)[0]
	r.wait(id, "two finished parts", func(j queue.JobView) bool { return j.PartsDone >= 2 })

	second := &App{ctx: context.Background(), profiles: r.profile.manager}
	_, err := queue.Open(r.queueOptions(r.dir, rigOptions{}))
	if err == nil || !errors.Is(err, queue.ErrUnavailable) || !strings.Contains(err.Error(), "another copy") {
		t.Fatalf("second instance: %v", err)
	}
	second.queueErr = err.Error()
	if st := second.TransferStatus(); st.Available || !strings.Contains(st.Error, "another copy") {
		t.Fatalf("status of an app without a queue: %+v", st)
	}
	if _, err := second.QueueJobs(); err == nil {
		t.Fatal("an app without a queue listed jobs")
	}

	// The first instance's live upload was not aborted.
	r.waitState(id, "completed")
	r.assertVerified(key, tSize, tSeed)
}

// TestSubmitValidation: what the queue refuses, item by item and as a whole.
func TestSubmitValidation(t *testing.T) {
	r := newRig(t, rigOptions{})
	good := r.pattern("ok.bin", tSeed, tMiB)
	folder := filepath.Join(r.files, "shots")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := r.app.QueueUploads(queue.UploadRequest{
		ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Prefix: strings.TrimSuffix(tPrefix, "/"),
		Paths: []string{good, folder, filepath.Join(r.files, "gone.bin"), "relative.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 4 || res.Items[0].JobID == "" || res.Items[0].Error != "" || res.Queued() != 1 {
		t.Fatalf("items: %+v", res.Items)
	}
	// An empty folder is expanded to nothing: no job, no error, counted.
	if it := res.Items[1]; it.Kind != queue.KindFolder || it.Queued != 0 || it.Error != "" || it.EmptyFolders != 1 || it.RefusedTotal != 0 {
		t.Fatalf("empty folder: %+v", it)
	}
	for i, want := range []string{"cannot read", "not absolute"} {
		if it := res.Items[i+2]; it.JobID != "" || !strings.Contains(it.Error, want) {
			t.Fatalf("item %d: %+v, want an error with %q", i+2, it, want)
		}
	}
	r.waitState(res.Items[0].JobID, "completed")
	r.assertVerified(tPrefix+"ok.bin", tMiB, tSeed) // A prefix without a slash got one.

	// Downloads: names that Windows cannot hold or that leave the folder.
	dest := t.TempDir()
	keys := []string{tPrefix + "folder/", tPrefix + "a:b.bin", tPrefix + "x/..", tPrefix + "con.txt", tPrefix + "trail.", tPrefix + "a\\b", ""}
	dres, err := r.app.QueueDownloads(queue.DownloadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Keys: keys, DestDir: dest})
	if err != nil {
		t.Fatal(err)
	}
	if len(dres.Items) != len(keys) || dres.Queued() != 0 {
		t.Fatalf("download items: %+v", dres.Items)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("a refused key wrote %d entries", len(entries))
	}
	if jobs, _ := r.app.QueueJobs(); len(jobs) != 1 {
		t.Fatalf("refused keys became jobs: %+v", jobs)
	}

	for name, call := range map[string]func() error{
		"no profile": func() error {
			_, e := r.app.QueueUploads(queue.UploadRequest{Bucket: "b", Region: "r", Paths: []string{good}})
			return e
		},
		"no bucket": func() error {
			_, e := r.app.QueueUploads(queue.UploadRequest{ProfileID: tProfile, Region: "r", Paths: []string{good}})
			return e
		},
		"unknown region": func() error {
			_, e := r.app.QueueUploads(queue.UploadRequest{ProfileID: tProfile, Bucket: "b", Paths: []string{good}})
			return e
		},
		"missing folder": func() error {
			_, e := r.app.QueueDownloads(queue.DownloadRequest{ProfileID: tProfile, Bucket: "b", Region: "r", Keys: []string{"k"}, DestDir: filepath.Join(dest, "nope")})
			return e
		},
		"relative folder": func() error {
			_, e := r.app.QueueDownloads(queue.DownloadRequest{ProfileID: tProfile, Bucket: "b", Region: "r", Keys: []string{"k"}, DestDir: "out"})
			return e
		},
	} {
		if call() == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestCloseWithRunningJobsLeavesNothing: closing the app while jobs
// run cancels them and leaves no multipart upload and no journal entry.
func TestCloseWithRunningJobsLeavesNothing(t *testing.T) {
	r := newRig(t, rigOptions{})
	for _, name := range []string{"a.bin", "b.bin"} {
		r.slowParts(tPrefix+name, 100*time.Millisecond)
	}
	ids := r.upload(r.pattern("a.bin", tSeed, tSize), r.pattern("b.bin", tSeed, tSize))
	for _, id := range ids {
		r.wait(id, "progress", func(j queue.JobView) bool { return j.PartsDone >= 1 })
	}
	r.app.closeQueue()
	r.assertNoUploads()
	if left := journalEntries(t, r.dir); len(left) != 0 {
		t.Fatalf("journal after close: %+v", left)
	}
	if _, err := r.app.QueueJobs(); err == nil {
		t.Fatal("a closed queue listed jobs")
	}
}

// TestSecondDownloadToSameDestinationIsRefused is review finding F6: a second
// request for a destination an unfinished download already has is refused at
// once, ignoring case, instead of downloading everything and failing at the end.
func TestSecondDownloadToSameDestinationIsRefused(t *testing.T) {
	r := newRig(t, rigOptions{})
	key := tPrefix + "clip.bin"
	if err := r.server.PutSynthetic(fakes3.DefaultBucket, key, tSeed, tSize); err != nil {
		t.Fatal(err)
	}
	if err := r.server.PutSynthetic(fakes3.DefaultBucket, tPrefix+"other/CLIP.bin", tSeed, tSize); err != nil {
		t.Fatal(err)
	}
	r.server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Key: key, Fault: fakes3.SlowBody(4 << 20)})
	dest := t.TempDir()
	first := r.download(dest, key)[0]

	for _, k := range []string{key, tPrefix + "other/CLIP.bin"} {
		res, err := r.app.QueueDownloads(queue.DownloadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region,
			Keys: []string{k}, DestDir: dest})
		if err != nil {
			t.Fatal(err)
		}
		if it := res.Items[0]; it.JobID != "" || it.Code != fsx.CodeExists || !strings.Contains(it.Error, "already the destination") {
			t.Fatalf("%s: %+v", it.Source, it)
		}
	}
	r.server.ClearRules()
	r.waitState(first, "completed")
	assertFile(t, filepath.Join(dest, "clip.bin"), tSeed, tSize)
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Fatalf("the folder holds %d entries", len(entries))
	}
}
