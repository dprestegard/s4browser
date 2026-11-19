package faultsuite_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"s4browser/internal/creds"
	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

// Fake session keys, as an external sign-in tool writes them. The fake server
// accepts any signature, and rules with AccessKey expire one set.
const (
	oldKey  = "ASIAFAKEOLDSESSION01"
	newKey  = "ASIAFAKENEWSESSION02"
	session = "sess"
)

var sessionID = creds.AWSID(session)

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

// sessionProfile is a session-key profile in temporary AWS files, never ~/.aws.
type sessionProfile struct {
	t           *testing.T
	credentials string
	manager     *creds.Manager
}

func newSessionProfile(t *testing.T) *sessionProfile {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("[profile "+session+"]\nregion = "+fakes3.Region+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &sessionProfile{t: t, credentials: filepath.Join(dir, "credentials")}
	p.signIn(oldKey)
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

// signIn rewrites the credentials file, as a new sign-in does.
func (p *sessionProfile) signIn(accessKey string) {
	p.t.Helper()
	text := "[" + session + "]\naws_access_key_id = " + accessKey + "\naws_secret_access_key = fake-secret-" + accessKey +
		"\naws_session_token = fake-token-" + accessKey + "\n"
	if err := os.WriteFile(p.credentials, []byte(text), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// clients builds clients from Manager.Provider, as the app does, so the engine,
// the prober and cleanup share one credentials cache.
func (p *sessionProfile) clients(server *fakes3.Server) s3x.ClientFunc {
	return s3x.NewClients(p.manager.Provider, endpointOptions(server.Endpoint(), server.CertPEM()))
}

func sessionRef(k string) engine.ObjectRef {
	return engine.ObjectRef{Profile: sessionID, Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: k}
}

func (r *rig) uploadAs(ref engine.ObjectRef) engine.JobID {
	r.t.Helper()
	id, err := r.eng.Submit(context.Background(), engine.JobSpec{Direction: engine.Upload, Ref: ref, LocalPath: srcPath})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// requestsBy counts finished requests for op signed with accessKey that S3
// rejected with code.
func requestsBy(server *fakes3.Server, op fakes3.Op, accessKey, code string) int {
	n := 0
	for _, rec := range server.Requests() {
		if rec.Op == op && rec.AccessKeyID == accessKey && rec.Code == code {
			n++
		}
	}
	return n
}

// TestSessionExpiresDuringUpload is the stage 1.1 risk: the session's file
// does not change when it expires, so loading it again still succeeds. The job
// must keep waiting until a new sign-in, and must then finish without sending
// any finished part again.
func TestSessionExpiresDuringUpload(t *testing.T) {
	server, guard := newServer(t)
	profile := newSessionProfile(t)
	clients := profile.clients(server)
	k := key("session.bin")
	app := launch(t, server, guard, t.TempDir(), rigOptions{
		clients: clients, prober: profile.manager.Prober(clients),
		settings: func(s *engine.Settings) { s.MaxPartsPerFile = 4 },
	})
	id := app.uploadAs(sessionRef(k))
	app.waitFor(id, "three finished parts", func(job engine.JobSnapshot) bool { return job.PartsDone >= 3 })
	server.AddRule(fakes3.Rule{AccessKey: oldKey, Fault: fakes3.ExpiredToken()})
	app.waitState(id, engine.JobCredentialWait)

	// Probes reload the unchanged file, which succeeds, and S3 still refuses it.
	app.waitFor(id, "probes refused by S3", func(engine.JobSnapshot) bool {
		return requestsBy(server, fakes3.OpGet, oldKey, "ExpiredToken") >= 3
	})
	if job := app.snapshot(id); job.State != engine.JobCredentialWait {
		t.Fatalf("an expired session that still loads was taken for recovery: %+v", job)
	}

	// A new sign-in. Nobody tells the app: the next probe reads the new file.
	profile.signIn(newKey)
	job := app.waitState(id, engine.JobCompleted)
	if job.Retries != 0 {
		t.Fatalf("credential failures were charged as retries: %d", job.Retries)
	}
	assertVerified(t, server, k)
	assertEachPartStoredOnce(t, server, k)
	app.assertClean()
}

// TestReloadResumesWaitingJobAtOnce: with probes far apart, a Reload after the
// new sign-in is what resumes the job, through OnCredentialsChanged and
// NotifyCredentialsChanged.
func TestReloadResumesWaitingJobAtOnce(t *testing.T) {
	server, guard := newServer(t)
	profile := newSessionProfile(t)
	clients := profile.clients(server)
	k := key("reload.bin")
	app := launch(t, server, guard, t.TempDir(), rigOptions{
		clients: clients, prober: profile.manager.Prober(clients),
		timeouts: func(to *engine.Timeouts) { to.ProbeMin, to.ProbeMax = time.Hour, time.Hour },
	})
	profile.manager.OnCredentialsChanged(app.eng.NotifyCredentialsChanged)
	server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: k, Part: 2, AccessKey: oldKey, Fault: fakes3.ExpiredToken()})
	id := app.uploadAs(sessionRef(k))
	app.waitState(id, engine.JobCredentialWait)

	profile.signIn(newKey)
	started := time.Now()
	if _, err := profile.manager.Reload(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	app.waitState(id, engine.JobCompleted)
	if waited := time.Since(started); waited > 30*time.Second {
		t.Fatalf("resuming took %v", waited)
	}
	assertVerified(t, server, k)
	app.assertClean()
}

// TestLaunchCleanupWaitsForCredentials: an upload left by the previous session
// cannot be aborted while the session is expired. The entry is kept and is
// cleaned up as soon as the credentials change, without another launch.
func TestLaunchCleanupWaitsForCredentials(t *testing.T) {
	server, guard := newServer(t)
	profile := newSessionProfile(t)
	k := key("left-behind.bin")
	dir := t.TempDir()

	// The previous session: an upload with a recorded ID, then a crash.
	uploadID := createUpload(t, server, k)
	previous := openJournal(t, dir)
	entry := journal.Entry{ID: "0123456789abcdef-1", Kind: journal.KindMultipartUpload, Phase: journal.PhaseActive,
		CreatedAt: time.Now().UTC(), Profile: sessionID, Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: k, UploadID: uploadID}
	if err := previous.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	previous.Close()

	server.AddRule(fakes3.Rule{AccessKey: oldKey, Fault: fakes3.ExpiredToken()})
	clients := profile.clients(server)
	app := launch(t, server, guard, dir, rigOptions{clients: clients, prober: profile.manager.Prober(clients)})
	var reports []journal.Report
	profile.manager.OnCredentialsChanged(func(id string) {
		app.eng.NotifyCredentialsChanged(id)
		report, err := app.recovery.RecoverProfile(context.Background(), id)
		if err != nil {
			t.Error(err)
		}
		reports = append(reports, report)
	})

	report := app.recover()
	if report.Deferred != 1 || report.UploadsAborted != 0 || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], sessionID) {
		t.Fatalf("report %+v", report)
	}
	if requestsBy(server, fakes3.OpAbort, oldKey, "ExpiredToken") != 1 {
		t.Fatal("a credential failure must not be retried at once")
	}
	if len(server.ActiveUploads(fakes3.DefaultBucket)) != 1 || len(app.journal.List()) != 1 {
		t.Fatal("the entry or the upload is gone")
	}

	profile.signIn(newKey)
	if _, err := profile.manager.Reload(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].UploadsAborted != 1 || reports[0].Deferred != 0 {
		t.Fatalf("reports %+v", reports)
	}
	app.assertClean()
}

func createUpload(t *testing.T, server *fakes3.Server, k string) string {
	t.Helper()
	id, err := server.Store().CreateMultipartUpload(context.Background(), refFor(k))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestProberRequiresS3ToAcceptCredentials covers each answer the production
// prober can get.
func TestProberRequiresS3ToAcceptCredentials(t *testing.T) {
	server, _ := newServer(t)
	profile := newSessionProfile(t)
	prober := profile.manager.Prober(profile.clients(server))
	var notified atomic.Int32
	profile.manager.OnCredentialsChanged(func(string) { notified.Add(1) })
	ref := sessionRef(key("missing-object.bin"))
	ctx := context.Background()

	expired := server.AddRule(fakes3.Rule{AccessKey: oldKey, Fault: fakes3.ExpiredToken()})
	err := prober.Probe(ctx, ref)
	if engine.Classify(err) != engine.ClassCredential {
		t.Fatalf("an expired session passed or was misread: %v", err)
	}
	server.RemoveRule(expired)
	if err := prober.Probe(ctx, ref); err != nil {
		t.Fatalf("NoSuchKey proves the credentials: %v", err)
	}

	for name, fault := range map[string]fakes3.Fault{
		"access denied": fakes3.AccessDenied(),
		"server error":  fakes3.ServiceUnavailable(),
		"reset":         fakes3.ResetAt(0),
		"invalid key":   fakes3.InvalidAccessKeyID(),
	} {
		rule := server.AddRule(fakes3.Rule{Op: fakes3.OpGet, Fault: fault})
		err := prober.Probe(ctx, ref)
		server.RemoveRule(rule)
		wantPass := name == "access denied"
		if (err == nil) != wantPass {
			t.Errorf("%s: probe returned %v, want pass %v", name, err, wantPass)
		}
	}

	// Credentials that do not load: no request is sent.
	before := len(server.Requests())
	if err := os.WriteFile(profile.credentials, []byte("[other]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prober.Probe(ctx, ref); !errors.Is(err, engine.ErrCredentialsUnavailable) {
		t.Fatalf("got %v", err)
	}
	if after := len(server.Requests()); after != before {
		t.Fatalf("%d requests sent without credentials", after-before)
	}
	if notified.Load() != 0 {
		t.Fatal("a probe announced a credential change, which would start another probe")
	}
}

var _ = http.StatusOK
