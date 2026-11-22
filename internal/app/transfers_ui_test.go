package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/browse"
	"s4browser/internal/fakes3"
	"s4browser/internal/queue"
)

// withBrowse gives the rig's app a browse service on the same fake S3, as the
// real app has, so listing selections can be downloaded.
func (r *rig) withBrowse() {
	r.t.Helper()
	b, err := browse.New(browse.Options{
		Dir:         r.t.TempDir(),
		Credentials: r.profile.manager.Provider,
		ClientOptions: []func(*s3.Options){func(so *s3.Options) {
			so.BaseEndpoint = aws.String(r.server.Endpoint())
			so.UsePathStyle = true
			so.HTTPClient = r.server.HTTPClient()
		}},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.app.browse = b
	r.t.Cleanup(b.Close)
}

// openListing opens a prefix through the binding and waits for it to finish.
func (r *rig) openListing(prefix string) browse.Snapshot {
	r.t.Helper()
	snap, err := r.app.OpenListing(tProfile, fakes3.DefaultBucket, prefix)
	if err != nil {
		r.t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for snap.State == browse.StateLoading {
		if time.Now().After(deadline) {
			r.t.Fatalf("listing %s did not finish: %+v", prefix, snap)
		}
		time.Sleep(10 * time.Millisecond)
		w, err := r.app.ListingWindow(snap.ID, "name", false, 0, 0)
		if err != nil {
			r.t.Fatal(err)
		}
		snap = w.Listing
	}
	return snap
}

// TestDownloadSelectionAndReports: the UI's bindings. A listing selection made
// of index ranges is resolved in Go, folders included, and the outcome comes
// back as a bounded report.
func TestDownloadSelectionAndReports(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.withBrowse()
	src := filepath.Join(r.files, "src")
	seeds := r.tree(src, map[string]int64{"a.bin": 2 * tMiB, "b.bin": 0, "sub/c.bin": 7 * tMiB})

	// Upload through StartUploads: the folder, a missing path and a relative one.
	rep, err := r.app.StartUploads(queue.UploadRequest{
		ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Prefix: tPrefix,
		Paths: []string{src, filepath.Join(r.files, "missing.bin"), "relative.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Requested != 3 || rep.Queued != 3 || rep.RefusedTotal != 2 || len(rep.Refused) != 2 {
		t.Fatalf("upload report = %+v, want 3 queued and 2 refused (missing, relative)", rep)
	}
	codes := map[string]string{}
	for _, f := range rep.Refused {
		codes[filepath.Base(f.Path)] = f.Code
	}
	if codes["missing.bin"] != queue.CodeNotFound || codes["relative.bin"] != "relative" {
		t.Fatalf("refusal codes = %v", codes)
	}
	r.waitAll()
	sizes := map[string]int64{"a.bin": 2 * tMiB, "b.bin": 0, "sub/c.bin": 7 * tMiB}
	for name, size := range sizes {
		r.assertVerified(tPrefix+"src/"+name, size, seeds[name])
	}

	// Select the one row of the prefix listing: the folder "src/". of the prefix listing: the folder "src/" and nothing else.
	top := r.openListing(tPrefix)
	if top.Count != 1 || top.Folders != 1 {
		t.Fatalf("listing of %s = %+v, want the one folder", tPrefix, top)
	}
	dest := t.TempDir()
	sel := DownloadSelection{
		ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region,
		ListingID: top.ID, Field: "name", Version: top.Version, Ranges: [][2]int{{0, 0}}, DestDir: dest,
	}
	rep, err = r.app.StartDownloads(sel)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Requested != 1 || rep.Queued != 3 || rep.RefusedTotal != 0 {
		t.Fatalf("download report = %+v, want the folder's 3 files", rep)
	}
	r.waitAll()
	for name, size := range sizes {
		assertFile(t, filepath.Join(dest, "src", filepath.FromSlash(name)), seeds[name], size)
	}

	// Rows of an inner listing, sorted by size descending: c's folder "sub/" sorts
	// first, then a.bin, then b.bin. Ranges [1,2] are the two files. They exist
	// now, so both are refused and nothing is replaced.
	inner := r.openListing(tPrefix + "src/")
	sel.ListingID, sel.Field, sel.Desc, sel.Version, sel.Ranges = inner.ID, "size", true, inner.Version, [][2]int{{1, 2}}
	sel.DestDir = filepath.Join(dest, "src")
	rep, err = r.app.StartDownloads(sel)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Queued != 0 || rep.RefusedTotal != 2 || rep.Refused[0].Code != "exists" || !strings.HasSuffix(rep.Refused[0].Path, "a.bin") {
		t.Fatalf("download over existing files: %+v", rep)
	}

	// A selection made on an older version, an empty one and a closed listing.
	sel.Version++
	if _, err := r.app.StartDownloads(sel); !errors.Is(err, browse.ErrSelectionChanged) {
		t.Fatalf("stale selection: err = %v", err)
	}
	sel.Version, sel.Ranges = inner.Version, nil
	if _, err := r.app.StartDownloads(sel); err == nil || !strings.Contains(err.Error(), "nothing is selected") {
		t.Fatalf("empty selection: err = %v", err)
	}
	r.app.CloseListing(inner.ID)
	sel.Ranges = [][2]int{{0, 0}}
	if _, err := r.app.StartDownloads(sel); err == nil || !strings.Contains(err.Error(), "select the items again") {
		t.Fatalf("closed listing: err = %v", err)
	}

	// Closing with nothing unfinished does not ask.
	if r.app.beforeClose(r.app.ctx) {
		t.Fatal("beforeClose kept the window open with no unfinished transfers")
	}
	if n, err := r.app.queue.Unfinished(r.app.ctx); err != nil || n != 0 {
		t.Fatalf("Unfinished = %d, %v", n, err)
	}
}

// TestUnfinishedCountsWaitingJobs: the count that makes quitting ask.
func TestUnfinishedCountsWaitingJobs(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := r.pattern("slow.bin", tSeed, tSize)
	r.slowParts(tPrefix+"slow.bin", time.Hour)
	id := r.upload(src)[0]
	r.waitState(id, "running")
	if err := r.app.PauseJob(id); err != nil {
		t.Fatal(err)
	}
	r.waitState(id, "paused")
	if n, err := r.app.queue.Unfinished(r.app.ctx); err != nil || n != 1 {
		t.Fatalf("Unfinished with a paused job = %d, %v", n, err)
	}
	if err := r.app.CancelJob(id); err != nil {
		t.Fatal(err)
	}
	r.waitState(id, "cancelled")
	if n, _ := r.app.queue.Unfinished(r.app.ctx); n != 0 {
		t.Fatalf("Unfinished after cancel = %d", n)
	}
}

// waitAll waits until no job is unfinished and none failed.
func (r *rig) waitAll() {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		jobs, err := r.app.QueueJobs()
		if err != nil {
			r.t.Fatal(err)
		}
		pending := 0
		for _, j := range jobs {
			if j.State == "failed" {
				r.t.Fatalf("job failed: %+v", j)
			}
			if !j.Finished {
				pending++
			}
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d jobs still unfinished", pending)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
