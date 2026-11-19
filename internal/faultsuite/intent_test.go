package faultsuite_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
)

// leaveIntent writes what a crash between Create and recording the upload ID
// leaves: a creating entry in the journal and an upload on S3 for key k.
func leaveIntent(t *testing.T, server *fakes3.Server, dir, k string) string {
	t.Helper()
	store := openJournal(t, dir)
	defer store.Close()
	entry := journal.Entry{ID: "fedcba9876543210-1", Kind: journal.KindMultipartUpload, Phase: journal.PhaseCreating,
		CreatedAt: time.Now().UTC(), Profile: profile, Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: k}
	if err := store.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	return createUpload(t, server, k)
}

func uploadIDs(server *fakes3.Server) map[string]bool {
	ids := map[string]bool{}
	for _, u := range server.ActiveUploads(fakes3.DefaultBucket) {
		ids[u.UploadID] = true
	}
	return ids
}

// TestLaunchCleanupSparesNewUploadOfSameKey: after a relaunch the user uploads
// the same file again, within the hour. The orphan and the new upload both
// match the old intent by key and time. Cleanup must leave the new one alone,
// and finish once the new job no longer has an upload in progress.
func TestLaunchCleanupSparesNewUploadOfSameKey(t *testing.T) {
	server, guard := newServer(t)
	k := key("same-key.bin")
	dir := t.TempDir()
	orphan := leaveIntent(t, server, dir, k)

	app := launch(t, server, guard, dir, rigOptions{timeouts: func(to *engine.Timeouts) {
		to.Stall, to.Response = time.Second, 2*time.Second
	}})
	stallParts(server, k, 5)
	id := app.upload(k)
	app.waitFor(id, "the new upload in progress", func(job engine.JobSnapshot) bool { return job.PartsDone >= 3 })
	if ids := uploadIDs(server); len(ids) != 2 || !ids[orphan] {
		t.Fatalf("uploads %v", ids)
	}

	report := app.recover()
	if report.UploadsAborted != 0 || report.Deferred != 1 || len(report.Warnings) != 1 {
		t.Fatalf("report %+v", report)
	}
	if ids := uploadIDs(server); len(ids) != 2 {
		t.Fatalf("an upload was aborted: %v", ids)
	}

	server.ClearRules()
	app.waitState(id, engine.JobCompleted)
	assertVerified(t, server, k)

	report = app.recover()
	if report.UploadsAborted != 1 || report.Deferred != 0 {
		t.Fatalf("report %+v", report)
	}
	app.assertClean()
}

// TestLaunchCleanupRacesNewUploads runs cleanup over and over while new jobs to
// the same keys start, upload and finish, so that every interleaving of the
// listing with a job's intent, Create and journal writes gets a chance. No new
// upload may be aborted: each job would then fail with NoSuchUpload.
func TestLaunchCleanupRacesNewUploads(t *testing.T) {
	server, guard := newServer(t)
	dir := t.TempDir()
	const jobs = 6
	keys := make([]string, jobs)
	for i := range keys {
		keys[i] = key("race/" + string(rune('a'+i)) + ".bin")
	}
	store := openJournal(t, dir)
	for i, k := range keys {
		entry := journal.Entry{ID: "00000000000000a" + string(rune('0'+i)) + "-1", Kind: journal.KindMultipartUpload, Phase: journal.PhaseCreating,
			CreatedAt: time.Now().UTC(), Profile: profile, Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: k}
		if err := store.Put(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
		createUpload(t, server, k)
	}
	store.Close()

	app := launch(t, server, guard, dir, rigOptions{settings: func(s *engine.Settings) { s.MaxActiveFiles = jobs }})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := app.recovery.Recover(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	ids := make([]engine.JobID, jobs)
	for i, k := range keys {
		ids[i] = app.upload(k)
	}
	for _, id := range ids {
		app.waitState(id, engine.JobCompleted)
	}
	close(stop)
	wg.Wait()

	for _, k := range keys {
		assertVerified(t, server, k)
	}
	app.recover()
	app.assertClean()
}
