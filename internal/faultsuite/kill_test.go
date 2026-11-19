package faultsuite_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/fsx"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

// childEnv makes this test binary act as the app in a separate process, so the
// tests can kill it for real (TerminateProcess on Windows): no deferred
// function, no engine Close and no journal Close runs.
const childEnv = "S4_FAULTSUITE_CHILD"

// Child modes.
const (
	// childUpload loses the first Create response, then uploads until parts
	// from 5 on stall. The journal holds the orphan's intent and the live upload.
	childUpload = "upload"
	// childCreate blocks the journal write that would record the upload ID, so
	// the process dies after Create succeeded and before its ID was durable.
	childCreate = "create"
	// childDownload downloads to a real file until ranges from part 4 on stall.
	childDownload = "download"
)

type childConfig struct {
	Mode       string `json:"mode"`
	Endpoint   string `json:"endpoint"`
	CertPEM    []byte `json:"certPem"`
	JournalDir string `json:"journalDir"`
	Key        string `json:"key"`
	Dest       string `json:"dest,omitempty"`
}

func TestMain(m *testing.M) {
	if raw := os.Getenv(childEnv); raw != "" {
		os.Exit(runChild(raw))
	}
	os.Exit(m.Run())
}

// runChild is the app being killed. It prints "ready" once the state the test
// wants is on disk and on S3, then waits to be killed.
func runChild(raw string) int {
	fail := func(format string, args ...any) int {
		fmt.Printf("error: "+format+"\n", args...)
		return 3
	}
	var cfg childConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return fail("config: %v", err)
	}
	store, err := journal.OpenFile(cfg.JournalDir)
	if err != nil {
		return fail("journal: %v", err)
	}
	guard, err := engine.NewTestPrefixGuard(runID)
	if err != nil {
		return fail("guard: %v", err)
	}
	clients := fakeKeyClients(cfg.Endpoint, cfg.CertPEM)
	source := fakes3.NewSource()
	source.Set(srcPath, fakes3.SyntheticFile{Size: objSize, Seed: seed})

	blocked := make(chan struct{})
	var journalStore journal.Store = store
	if cfg.Mode == childCreate {
		journalStore = &blockActive{Store: store, blocked: blocked}
	}
	settings := engine.DefaultSettings()
	settings.PartSize = partSize
	eng, err := engine.New(engine.Config{
		Store: fakes3.GuardStore(s3x.NewStore(clients), guard), Source: source, Sink: engine.NewFileSink(fsx.DefaultStrategy()),
		Journal: journalStore, Prober: passProber{}, Clock: engine.RealClock{}, Guard: guard,
		Jitter: func() float64 { return 0.5 }, Settings: settings, Retry: engine.DefaultRetryPolicy(), Timeouts: engine.DefaultTimeouts(),
	})
	if err != nil {
		return fail("engine: %v", err)
	}
	spec := engine.JobSpec{Direction: engine.Upload, Ref: refFor(cfg.Key), LocalPath: srcPath}
	if cfg.Mode == childDownload {
		spec = engine.JobSpec{Direction: engine.Download, Ref: refFor(cfg.Key), LocalPath: cfg.Dest}
	}
	id, err := eng.Submit(context.Background(), spec)
	if err != nil {
		return fail("submit: %v", err)
	}

	ready := func() bool {
		all, err := eng.Snapshot(context.Background())
		if err != nil || len(all) != 1 {
			return false
		}
		job := all[0]
		if job.State.IsTerminal() {
			fmt.Printf("error: job ended %s: %+v\n", job.State, job.Err)
			os.Exit(4)
		}
		switch cfg.Mode {
		case childCreate:
			select {
			case <-blocked:
				return true
			default:
				return false
			}
		case childDownload:
			_, statErr := os.Stat(fsx.TempPath(cfg.Dest, string(id)))
			return job.PartsDone >= 2 && statErr == nil && hasKind(store.List(), journal.KindDownloadTemp, journal.PhaseActive)
		default:
			return job.PartsDone >= 3 && hasKind(store.List(), journal.KindMultipartUpload, journal.PhaseActive)
		}
	}
	for !ready() {
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Println("ready")
	time.Sleep(10 * time.Minute) // Killed long before this.
	return 5
}

func hasKind(entries []journal.Entry, kind journal.Kind, phase journal.Phase) bool {
	for _, e := range entries {
		if e.Kind == kind && e.Phase == phase {
			return true
		}
	}
	return false
}

// blockActive never lets the upload ID become durable.
type blockActive struct {
	journal.Store
	blocked chan struct{}
	once    sync.Once
}

func (b *blockActive) Put(ctx context.Context, e journal.Entry) error {
	if e.Kind == journal.KindMultipartUpload && e.Phase == journal.PhaseActive {
		b.once.Do(func() { close(b.blocked) })
		select {} // The process is killed here.
	}
	return b.Store.Put(ctx, e)
}

// runAndKill starts the child, waits for "ready", proves the journal is locked
// while it lives, then kills it.
func runAndKill(t *testing.T, cfg childConfig) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), childEnv+"="+string(raw))
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()

	lines := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			line = "error: " + err.Error()
		}
		lines <- strings.TrimSpace(line)
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case line := <-lines:
		if line != "ready" {
			t.Fatalf("child said %q", line)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the child never became ready")
	}

	if second, err := journal.OpenFile(cfg.JournalDir); !errors.Is(err, journal.ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("a second instance opened the live journal: %v", err)
	}

	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	killed = true
}

func childFor(server *fakes3.Server, mode, dir, k string) childConfig {
	return childConfig{Mode: mode, Endpoint: server.Endpoint(), CertPEM: server.CertPEM(), JournalDir: dir, Key: k}
}

// TestKillDuringUploadThenRelaunch kills the app while parts upload, after a
// lost Create response left an orphan. The next launch aborts both uploads,
// empties the journal, and the same file then uploads normally.
func TestKillDuringUploadThenRelaunch(t *testing.T) {
	server, guard := newServer(t)
	k := key("killed-upload.bin")
	dir := t.TempDir()
	server.AddRule(fakes3.Rule{Op: fakes3.OpCreateUpload, Key: k, Attempt: 1, Fault: fakes3.LostResponse()})
	stallParts(server, k, 5)

	runAndKill(t, childFor(server, childUpload, dir, k))

	uploads := server.ActiveUploads(fakes3.DefaultBucket)
	if len(uploads) != 2 {
		t.Fatalf("want the orphan and the live upload, got %+v", uploads)
	}
	server.ClearRules()

	app := launch(t, server, guard, dir, rigOptions{})
	entries := app.journal.List()
	if len(entries) != 2 || !hasKind(entries, journal.KindMultipartUpload, journal.PhaseActive) ||
		!hasKind(entries, journal.KindMultipartUpload, journal.PhaseCreating) {
		t.Fatalf("journal after the kill: %+v", entries)
	}
	report := app.recover()
	if report.UploadsAborted != 2 || report.Deferred != 0 || len(report.Warnings) != 0 {
		t.Fatalf("report %+v", report)
	}
	app.assertClean()

	id := app.upload(k)
	app.waitState(id, engine.JobCompleted)
	assertVerified(t, server, k)
	app.assertClean()
	app.shutdown()

	// The cleanup is durable: a third launch finds nothing.
	again := openJournal(t, dir)
	defer again.Close()
	if entries := again.List(); len(entries) != 0 {
		t.Fatalf("entries after reopening: %+v", entries)
	}
}

// TestKillAfterCreateBeforeIDIsDurable kills the app after S3 created the upload
// and before its ID reached the journal. Only the intent is known, so launch
// cleanup finds the upload by key and time.
func TestKillAfterCreateBeforeIDIsDurable(t *testing.T) {
	server, guard := newServer(t)
	k := key("killed-create.bin")
	dir := t.TempDir()

	runAndKill(t, childFor(server, childCreate, dir, k))

	uploads := server.ActiveUploads(fakes3.DefaultBucket)
	if len(uploads) != 1 || len(uploads[0].Parts) != 0 {
		t.Fatalf("want one empty upload, got %+v", uploads)
	}
	app := launch(t, server, guard, dir, rigOptions{})
	if entries := app.journal.List(); len(entries) != 1 || entries[0].Phase != journal.PhaseCreating {
		t.Fatalf("journal after the kill: %+v", entries)
	}
	if report := app.recover(); report.UploadsAborted != 1 || report.Deferred != 0 {
		t.Fatalf("report %+v", report)
	}
	app.assertClean()
}

// TestKillDuringDownloadThenRelaunch kills the app while it writes a large
// download. The next launch deletes the temp file and the entry, and a new
// download of the object then succeeds.
func TestKillDuringDownloadThenRelaunch(t *testing.T) {
	server, guard := newServer(t)
	k := key("killed-download.bin")
	if err := server.PutSynthetic(fakes3.DefaultBucket, k, seed, objSize); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	downloads := t.TempDir()
	dest := filepath.Join(downloads, "movie.bin")
	stallRanges(server, k, 4)

	cfg := childFor(server, childDownload, dir, k)
	cfg.Dest = dest
	runAndKill(t, cfg)

	names := listDir(t, downloads)
	if len(names) != 1 || !fsx.IsTempPath(names[0]) {
		t.Fatalf("want one temp file after the kill, got %v", names)
	}
	server.ClearRules()

	app := launch(t, server, guard, dir, rigOptions{sink: engine.NewFileSink(fsx.DefaultStrategy())})
	if entries := app.journal.List(); len(entries) != 1 || entries[0].Kind != journal.KindDownloadTemp {
		t.Fatalf("journal after the kill: %+v", entries)
	}
	if report := app.recover(); report.TempsRemoved != 1 || report.Deferred != 0 {
		t.Fatalf("report %+v", report)
	}
	if names := listDir(t, downloads); len(names) != 0 {
		t.Fatalf("files remain: %v", names)
	}
	app.assertClean()

	id := app.download(k, dest)
	app.waitState(id, engine.JobCompleted)
	if names := listDir(t, downloads); len(names) != 1 || names[0] != "movie.bin" {
		t.Fatalf("files after the new download: %v", names)
	}
	verifyFile(t, dest)
	app.assertClean()
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func verifyFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pattern := fakes3.Pattern{Seed: seed}
	buf := make([]byte, mib)
	scratch := make([]byte, mib)
	var off int64
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			if at, ok := pattern.Equal(buf[:n], off, scratch); !ok {
				t.Fatalf("content differs at byte %d", at)
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if off != objSize {
		t.Fatalf("file is %d bytes, want %d", off, objSize)
	}
}
