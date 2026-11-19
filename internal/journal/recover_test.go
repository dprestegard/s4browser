package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCleaner is an in-memory S3 for launch cleanup. Uploads are keyed by ID.
type fakeCleaner struct {
	mu      sync.Mutex
	uploads map[string]RemoteUpload
	aborted []string
	// failures returns an error for a call, or nil. op is "abort" or "list".
	failures func(op, uploadID string) error
	// onList runs after the listing is taken and before it is returned.
	onList func()

	active, peak int
	delay        time.Duration
}

func newFakeCleaner(uploads ...RemoteUpload) *fakeCleaner {
	c := &fakeCleaner{uploads: make(map[string]RemoteUpload)}
	for _, u := range uploads {
		c.uploads[u.UploadID] = u
	}
	return c
}

func (c *fakeCleaner) enter() {
	c.mu.Lock()
	c.active++
	c.peak = max(c.peak, c.active)
	delay := c.delay
	c.mu.Unlock()
	time.Sleep(delay)
}

func (c *fakeCleaner) leave() {
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
}

func (c *fakeCleaner) AbortUpload(_ context.Context, _ Entry, uploadID string) error {
	c.enter()
	defer c.leave()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures != nil {
		if err := c.failures("abort", uploadID); err != nil {
			return err
		}
	}
	if _, ok := c.uploads[uploadID]; !ok {
		return ErrUploadGone
	}
	delete(c.uploads, uploadID)
	c.aborted = append(c.aborted, uploadID)
	return nil
}

func (c *fakeCleaner) ListUploads(_ context.Context, e Entry) ([]RemoteUpload, error) {
	c.enter()
	defer c.leave()
	c.mu.Lock()
	if c.failures != nil {
		if err := c.failures("list", ""); err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
	var out []RemoteUpload
	for _, u := range c.uploads {
		if strings.HasPrefix(u.Key, e.Key) {
			out = append(out, u)
		}
	}
	onList := c.onList
	c.mu.Unlock()
	if onList != nil {
		onList()
	}
	return out, nil
}

func (c *fakeCleaner) remaining() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for id := range c.uploads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func intentEntry(id, key string) Entry {
	return Entry{ID: id, Kind: KindMultipartUpload, Phase: PhaseCreating, CreatedAt: testTime,
		Profile: "work", Region: "us-east-1", Bucket: "media", Key: key}
}

func activeEntry(id, key, uploadID string) Entry {
	e := intentEntry(id, key)
	e.Phase, e.UploadID = PhaseActive, uploadID
	return e
}

// launch opens a journal holding entries, as a previous session left it, and
// reopens it, so that the store's launch set is exactly entries.
func launch(t *testing.T, entries ...Entry) *FileStore {
	t.Helper()
	dir := t.TempDir()
	first, err := OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := first.Put(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	first.Close()
	return openStore(t, dir)
}

func newRecovery(t *testing.T, store Store, cleaner RemoteCleaner) *Recovery {
	t.Helper()
	r, err := NewRecovery(RecoveryOptions{Store: store, Cleaner: cleaner, BaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ids(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func TestRecoverAbortsRecordedUploads(t *testing.T) {
	store := launch(t,
		activeEntry("a-1", "show/a.mkv", "up-a"),
		activeEntry("b-1", "show/b.mkv", "up-b-completed"), // Completed before the crash: S3 has no such upload.
	)
	cleaner := newFakeCleaner(RemoteUpload{Key: "show/a.mkv", UploadID: "up-a", Initiated: testTime})
	report, err := newRecovery(t, store, cleaner).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.UploadsAborted != 1 || report.Deferred != 0 || len(report.Warnings) != 0 {
		t.Fatalf("report %+v", report)
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("entries remain: %v", ids(got))
	}
	if len(cleaner.remaining()) != 0 {
		t.Fatalf("uploads remain: %v", cleaner.remaining())
	}
}

func TestRecoverIntentAbortsOnlyMatchingUploads(t *testing.T) {
	const key = "show/a.mkv"
	store := launch(t,
		intentEntry("a-1-c1", key),
		activeEntry("a-1", key, "up-recorded"),
	)
	cleaner := newFakeCleaner(
		RemoteUpload{Key: key, UploadID: "up-lost", Initiated: testTime.Add(time.Second)},
		RemoteUpload{Key: key, UploadID: "up-recorded", Initiated: testTime.Add(2 * time.Second)},
		RemoteUpload{Key: key, UploadID: "up-old", Initiated: testTime.Add(-IntentSkewBefore - time.Minute)},
		RemoteUpload{Key: key, UploadID: "up-later", Initiated: testTime.Add(IntentWindow + time.Minute)},
		RemoteUpload{Key: key + ".bak", UploadID: "up-other-key", Initiated: testTime},
	)
	report, err := newRecovery(t, store, cleaner).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.UploadsAborted != 2 || report.Deferred != 0 {
		t.Fatalf("report %+v", report)
	}
	want := []string{"up-later", "up-old", "up-other-key"}
	if got := cleaner.remaining(); !slices.Equal(got, want) {
		t.Fatalf("remaining %v, want %v", got, want)
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("entries remain: %v", ids(got))
	}
}

// TestRecoverLeavesThisSessionsUploadsAlone is the guard on intent matching: a
// new job uploading the same key within the hour must never be aborted.
func TestRecoverLeavesThisSessionsUploadsAlone(t *testing.T) {
	const key = "show/a.mkv"
	store := launch(t, intentEntry("old-1", key))
	cleaner := newFakeCleaner(RemoteUpload{Key: key, UploadID: "up-orphan", Initiated: testTime})
	recovery := newRecovery(t, store, cleaner)

	// A new job's intent, then its live upload, both inside the window.
	ctx := context.Background()
	session := intentEntry("new-1", key)
	session.CreatedAt = testTime.Add(10 * time.Minute)
	if err := store.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	cleaner.uploads["up-live"] = RemoteUpload{Key: key, UploadID: "up-live", Initiated: testTime.Add(10 * time.Minute)}

	report, err := recovery.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.UploadsAborted != 0 || report.Deferred != 1 || len(report.Warnings) != 1 {
		t.Fatalf("report %+v", report)
	}
	if got := cleaner.remaining(); !slices.Equal(got, []string{"up-live", "up-orphan"}) {
		t.Fatalf("remaining %v", got)
	}

	// The job records its upload ID, finishes and deletes its entry. Its upload
	// is no longer in progress, so the orphan can be found safely.
	active := activeEntry("new-1", key, "up-live")
	active.CreatedAt = session.CreatedAt
	if err := store.Put(ctx, active); err != nil {
		t.Fatal(err)
	}
	if report, _ := recovery.Recover(ctx); report.Deferred != 1 {
		t.Fatalf("an active session entry must also defer: %+v", report)
	}
	delete(cleaner.uploads, "up-live")
	if err := store.Delete(ctx, "new-1"); err != nil {
		t.Fatal(err)
	}
	report, err = recovery.Recover(ctx)
	if err != nil || report.UploadsAborted != 1 || report.Deferred != 0 {
		t.Fatalf("report %+v, %v", report, err)
	}
	if got := cleaner.remaining(); len(got) != 0 {
		t.Fatalf("remaining %v", got)
	}
}

// TestRecoverReadsJournalAfterListing covers a job that makes its intent durable
// and creates its upload while the listing is taken. The listing contains the
// new upload, so the journal must be read after it.
func TestRecoverReadsJournalAfterListing(t *testing.T) {
	const key = "show/a.mkv"
	store := launch(t, intentEntry("old-1", key))
	cleaner := newFakeCleaner()
	cleaner.onList = func() {
		session := intentEntry("new-1", key)
		session.CreatedAt = testTime.Add(time.Minute)
		if err := store.Put(context.Background(), session); err != nil {
			t.Error(err)
		}
	}
	// The listing sees the live upload: Put happens after the listing was taken,
	// but before Recovery reads the journal, which is the worst order.
	cleaner.uploads["up-live"] = RemoteUpload{Key: key, UploadID: "up-live", Initiated: testTime.Add(time.Minute)}

	report, err := newRecovery(t, store, cleaner).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.UploadsAborted != 0 || report.Deferred != 1 {
		t.Fatalf("report %+v", report)
	}
	if got := cleaner.remaining(); !slices.Equal(got, []string{"up-live"}) {
		t.Fatalf("the live upload was aborted: remaining %v", got)
	}
}

func TestRecoverNeverTouchesEntriesWrittenAfterLaunch(t *testing.T) {
	store := launch(t)
	ctx := context.Background()
	if err := store.Put(ctx, activeEntry("new-1", "show/a.mkv", "up-live")); err != nil {
		t.Fatal(err)
	}
	cleaner := newFakeCleaner(RemoteUpload{Key: "show/a.mkv", UploadID: "up-live", Initiated: testTime})
	// Created after the session's entry: the launch set still comes from what was loaded.
	report, err := newRecovery(t, store, cleaner).Recover(ctx)
	if err != nil || !reflect.DeepEqual(report, Report{}) {
		t.Fatalf("report %+v, %v", report, err)
	}
	if len(store.List()) != 1 || len(cleaner.remaining()) != 1 {
		t.Fatal("a session entry was processed")
	}
	if got := store.Loaded(); len(got) != 0 {
		t.Fatalf("Loaded includes later entries: %v", ids(got))
	}
}

func TestRecoverDefersOnCredentialsAndRetriesByProfile(t *testing.T) {
	store := launch(t,
		activeEntry("a-1", "show/a.mkv", "up-a"),
		intentEntry("b-1", "show/b.mkv"),
	)
	cleaner := newFakeCleaner(
		RemoteUpload{Key: "show/a.mkv", UploadID: "up-a", Initiated: testTime},
		RemoteUpload{Key: "show/b.mkv", UploadID: "up-b", Initiated: testTime},
	)
	calls := 0
	expired := true
	cleaner.failures = func(string, string) error {
		calls++
		if expired {
			return fmt.Errorf("%w: ExpiredToken", ErrCredentials)
		}
		return nil
	}
	recovery := newRecovery(t, store, cleaner)
	report, err := recovery.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Deferred != 2 || report.UploadsAborted != 0 || len(report.Warnings) != 2 {
		t.Fatalf("report %+v", report)
	}
	if calls != 2 {
		t.Fatalf("a credential failure was retried: %d calls", calls)
	}
	if len(store.List()) != 2 || len(recovery.Pending()) != 2 {
		t.Fatal("deferred entries were dropped")
	}
	if report, _ := recovery.RecoverProfile(context.Background(), "other"); !reflect.DeepEqual(report, Report{}) {
		t.Fatalf("another profile's pass touched entries: %+v", report)
	}

	expired = false
	report, err = recovery.RecoverProfile(context.Background(), "work")
	if err != nil || report.UploadsAborted != 2 || report.Deferred != 0 {
		t.Fatalf("report %+v, %v", report, err)
	}
	if len(store.List()) != 0 || len(cleaner.remaining()) != 0 {
		t.Fatal("cleanup did not finish after the credentials returned")
	}
}

func TestRecoverRetriesTransientFailures(t *testing.T) {
	store := launch(t, activeEntry("a-1", "show/a.mkv", "up-a"), intentEntry("b-1", "show/b.mkv"))
	cleaner := newFakeCleaner(
		RemoteUpload{Key: "show/a.mkv", UploadID: "up-a", Initiated: testTime},
		RemoteUpload{Key: "show/b.mkv", UploadID: "up-b", Initiated: testTime},
	)
	failed := map[string]int{}
	cleaner.failures = func(op, id string) error {
		if failed[op+id] < 2 {
			failed[op+id]++
			return errors.New("503 SlowDown")
		}
		return nil
	}
	report, err := newRecovery(t, store, cleaner).Recover(context.Background())
	if err != nil || report.UploadsAborted != 2 || report.Deferred != 0 {
		t.Fatalf("report %+v, %v", report, err)
	}

	// A failure that outlasts the attempts keeps the entry for next launch.
	store = launch(t, activeEntry("a-1", "show/a.mkv", "up-a"))
	cleaner = newFakeCleaner(RemoteUpload{Key: "show/a.mkv", UploadID: "up-a", Initiated: testTime})
	attempts := 0
	cleaner.failures = func(string, string) error { attempts++; return errors.New("connection reset") }
	report, err = newRecovery(t, store, cleaner).Recover(context.Background())
	if err != nil || report.Deferred != 1 || len(report.Warnings) != 1 {
		t.Fatalf("report %+v, %v", report, err)
	}
	if attempts != DefaultRecoveryAttempts {
		t.Fatalf("%d attempts, want %d", attempts, DefaultRecoveryAttempts)
	}
	if len(store.List()) != 1 {
		t.Fatal("the entry was dropped")
	}
}

func TestRecoverRemovesOnlyTempFiles(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "~s4-0123456789abcdef.s4tmp")
	other := filepath.Join(dir, "precious.mkv")
	for _, path := range []string{temp, other} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tempOf := func(id, path string) Entry {
		return Entry{ID: id, Kind: KindDownloadTemp, Phase: PhaseActive, CreatedAt: testTime, TempPath: path}
	}
	store := launch(t,
		tempOf("t-1", temp),
		tempOf("t-2", other),
		tempOf("t-3", filepath.Join(dir, "~s4-fedcba9876543210.s4tmp")), // Never created.
	)
	report, err := newRecovery(t, store, newFakeCleaner()).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.TempsRemoved != 1 || report.Deferred != 0 || len(report.Warnings) != 1 {
		t.Fatalf("report %+v", report)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a file that is not a temp file was touched: %v", err)
	}
	if len(store.List()) != 0 {
		t.Fatalf("entries remain: %v", ids(store.List()))
	}

	// A temp file that cannot be removed now stays journaled.
	store = launch(t, tempOf("t-1", temp))
	recovery, err := NewRecovery(RecoveryOptions{Store: store, Cleaner: newFakeCleaner(),
		RemoveFile: func(string) error { return errors.New("sharing violation") }})
	if err != nil {
		t.Fatal(err)
	}
	if report, _ := recovery.Recover(context.Background()); report.Deferred != 1 || len(store.List()) != 1 {
		t.Fatalf("report %+v", report)
	}
}

func TestRecoverBoundsParallelRequests(t *testing.T) {
	var entries []Entry
	var uploads []RemoteUpload
	for i := range 12 {
		key := fmt.Sprintf("show/%02d.mkv", i)
		entries = append(entries, activeEntry(fmt.Sprintf("j%02d-1", i), key, "up-"+key))
		uploads = append(uploads, RemoteUpload{Key: key, UploadID: "up-" + key, Initiated: testTime})
	}
	store := launch(t, entries...)
	cleaner := newFakeCleaner(uploads...)
	cleaner.delay = 20 * time.Millisecond
	report, err := newRecovery(t, store, cleaner).Recover(context.Background())
	if err != nil || report.UploadsAborted != 12 {
		t.Fatalf("report %+v, %v", report, err)
	}
	if cleaner.peak > DefaultRecoveryParallel || cleaner.peak < 2 {
		t.Fatalf("peak concurrency %d, limit %d", cleaner.peak, DefaultRecoveryParallel)
	}
}

func TestRecoverStopsWithContext(t *testing.T) {
	store := launch(t, activeEntry("a-1", "show/a.mkv", "up-a"))
	cleaner := newFakeCleaner(RemoteUpload{Key: "show/a.mkv", UploadID: "up-a", Initiated: testTime})
	ctx, cancel := context.WithCancel(context.Background())
	cleaner.failures = func(string, string) error { cancel(); return errors.New("timeout") }
	report, err := newRecovery(t, store, cleaner).Recover(ctx)
	if !errors.Is(err, context.Canceled) || report.Deferred != 1 || len(store.List()) != 1 {
		t.Fatalf("report %+v, %v", report, err)
	}
}
