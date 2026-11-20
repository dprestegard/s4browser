package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"s4browser/internal/browse"
	"s4browser/internal/fakes3"
	"s4browser/internal/objects"
)

// TestDeleteThroughBindings: a listing selection is counted in Go, confirmed by
// its plan ID and deleted once. Folders go with everything below them, and
// nothing outside the selection is touched.
func TestDeleteThroughBindings(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.withBrowse()
	var mu sync.Mutex
	var events []ObjectProgress
	r.app.emitHook = func(event string, payload any) {
		if event == "s4:objects" {
			mu.Lock()
			events = append(events, payload.(ObjectProgress))
			mu.Unlock()
		}
	}
	now := time.Now()
	for _, k := range []string{"dir/a.bin", "dir/sub/b.bin", "dir/", "dirx/keep.bin", "one.bin", "two.bin"} {
		size := int64(100)
		if strings.HasSuffix(k, "/") {
			size = 0
		}
		if err := r.server.PutListed(fakes3.DefaultBucket, tPrefix+k, size, now); err != nil {
			t.Fatal(err)
		}
	}
	// Name order: dir/, dirx/, one.bin, two.bin. Select dir/ and one.bin.
	top := r.openListing(tPrefix)
	sel := ObjectSelection{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, ListingID: top.ID, Field: "name", Version: top.Version, Ranges: [][2]int{{0, 0}, {2, 2}}}
	p, err := r.app.PrepareDelete(sel)
	if err != nil {
		t.Fatal(err)
	}
	if p.Objects != 4 || p.Folders != 1 || p.Selected != 2 || p.Placeholders != 1 || p.Bytes != 300 || p.Versioning != objects.VersioningOff || p.Prefix != tPrefix {
		t.Fatalf("preview = %+v", p)
	}
	if len(p.Sample) != 4 || p.Sample[0] != tPrefix+"dir/" {
		t.Fatalf("sample = %v", p.Sample)
	}
	if r.server.ObjectCount(fakes3.DefaultBucket) != 6 {
		t.Fatal("counting deleted something")
	}

	res, err := r.app.RunDelete(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requested != 4 || res.Deleted != 4 || res.FailedTotal != 0 || res.Stopped != "" {
		t.Fatalf("result = %+v", res)
	}
	for _, k := range []string{"dirx/keep.bin", "two.bin"} {
		if _, ok := r.server.Object(fakes3.DefaultBucket, tPrefix+k); !ok {
			t.Fatalf("%s was deleted", k)
		}
	}
	if r.server.ObjectCount(fakes3.DefaultBucket) != 2 {
		t.Fatalf("%d objects left", r.server.ObjectCount(fakes3.DefaultBucket))
	}
	mu.Lock()
	last := events[len(events)-1]
	mu.Unlock()
	if last.Phase != "deleting" || last.Done != 4 || last.Total != 4 {
		t.Fatalf("last event = %+v", last)
	}

	// A plan runs once.
	if _, err := r.app.RunDelete(p.ID); err == nil {
		t.Fatal("a plan ran twice")
	}
	// The listing is a snapshot, so the same selection still resolves: the
	// folder is empty now and only the selected file's key is counted again.
	// Deleting a key that is gone is harmless in S3.
	p2, err := r.app.PrepareDelete(sel)
	if err != nil || p2.Objects != 1 || p2.Folders != 1 {
		t.Fatalf("second count: %+v, %v", p2, err)
	}
	r.app.DiscardDelete(p2.ID)
	if _, err := r.app.RunDelete(p2.ID); err == nil {
		t.Fatal("a discarded plan ran")
	}
	sel.Version++
	if _, err := r.app.PrepareDelete(sel); !errors.Is(err, browse.ErrSelectionChanged) {
		t.Fatalf("stale selection: %v", err)
	}
	sel.Version, sel.Bucket = top.Version, "other-bucket"
	if _, err := r.app.PrepareDelete(sel); err == nil || !strings.Contains(err.Error(), "another bucket") {
		t.Fatalf("bucket mismatch: %v", err)
	}

	// Over the cap: refused while counting, nothing deleted.
	r.app.maxDelete = 1
	again := r.openListing(tPrefix)
	sel = ObjectSelection{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, ListingID: again.ID, Field: "name", Version: again.Version, Ranges: [][2]int{{0, 1}}}
	if _, err := r.app.PrepareDelete(sel); !errors.Is(err, objects.ErrTooMany) {
		t.Fatalf("over the cap: %v", err)
	}
	r.app.DiscardDelete("nope")
}

// TestObjectBindings: create folder, presign, metadata and tags through the
// bindings, with the guard proving writes stay in the test prefix.
func TestObjectBindings(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.withBrowse()
	bucket := fakes3.DefaultBucket

	key, err := r.app.CreateFolder(tProfile, bucket, tPrefix, "New Reel")
	if err != nil || key != tPrefix+"New Reel/" {
		t.Fatalf("create folder: %q, %v", key, err)
	}
	if _, err := r.app.CreateFolder(tProfile, bucket, tPrefix, "New Reel"); !errors.Is(err, objects.ErrFolderExists) {
		t.Fatalf("again: %v", err)
	}
	if _, err := r.app.CreateFolder(tProfile, bucket, "", "outside"); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("outside the test prefix: %v", err)
	}

	obj := tPrefix + "clip.mov"
	if err := r.server.PutSynthetic(bucket, obj, 3, 10<<30); err != nil {
		t.Fatal(err)
	}
	if err := r.server.SetExtras(bucket, obj, fakes3.ObjectExtras{Headers: map[string]string{"Content-Type": "video/quicktime"}, Meta: map[string]string{"camera": "B"}}); err != nil {
		t.Fatal(err)
	}
	m, err := r.app.ObjectMetadata(tProfile, bucket, obj)
	if err != nil || m.Size != 10<<30 || m.ContentType != "video/quicktime" || m.Metadata["camera"] != "B" {
		t.Fatalf("metadata = %+v, %v", m, err)
	}
	if _, err := r.app.ObjectMetadata(tProfile, bucket, tPrefix+"missing"); err == nil {
		t.Fatal("metadata of a missing object")
	}

	p, err := r.app.PresignObject(tProfile, bucket, obj, 3600)
	if err != nil || !strings.Contains(p.URL, "X-Amz-Expires=3600") || p.Effective != 3600 {
		t.Fatalf("presign = %+v, %v", p, err)
	}

	tags, err := r.app.ObjectTags(tProfile, bucket, obj)
	if err != nil || len(tags) != 0 {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	saved, err := r.app.SaveObjectTags(tProfile, bucket, obj, tags, []objects.Tag{{Key: "reel", Value: "2"}})
	if err != nil || fmt.Sprint(saved) != "[{reel 2}]" {
		t.Fatalf("saved = %v, %v", saved, err)
	}
	if _, err := r.app.SaveObjectTags(tProfile, bucket, obj, nil, []objects.Tag{{Key: "x", Value: "y"}}); !errors.Is(err, objects.ErrTagsChanged) {
		t.Fatalf("stale save: %v", err)
	}
	if _, err := r.app.SaveObjectTags(tProfile, bucket, obj, saved, []objects.Tag{{Key: "aws:x", Value: "y"}}); err == nil {
		t.Fatal("a reserved key was saved")
	}
}
