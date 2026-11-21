package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/objects"
)

func TestWriteGuardFromEnv(t *testing.T) {
	t.Setenv(writePrefixEnv, "upload-test/run-43/")
	g := writeGuardFromEnv()
	if g == nil || g.CheckWrite("upload-test/run-43/a.bin") != nil || !errors.Is(g.CheckWrite("upload-test/a.bin"), engine.ErrWriteRefused) {
		t.Fatalf("guard %v", g)
	}
	// A mistyped prefix refuses everything rather than turning the guard off.
	t.Setenv(writePrefixEnv, "upload-test")
	g = writeGuardFromEnv()
	if g == nil || !errors.Is(g.CheckWrite("upload-test/run-43/a.bin"), engine.ErrWriteRefused) {
		t.Fatalf("an invalid prefix allowed a write: %v", g)
	}
	a := &App{writeGuard: g}
	if a.queueOptions(t.TempDir()).Guard == nil {
		t.Fatal("the queue was not given the guard")
	}
	if (&App{}).queueOptions(t.TempDir()).Guard != nil {
		t.Fatal("a guard without S4_WRITE_PREFIX")
	}
}

// TestWriteGuardRefusesObjectWrites: with the app's guard set, folders, tags
// and deletes outside its prefix are refused before any request, and the same
// operations inside it work.
func TestWriteGuardRefusesObjectWrites(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.withBrowse()
	bucket := fakes3.DefaultBucket
	inside := tPrefix + "inside/"
	g, err := engine.NewPrefixGuard(inside)
	if err != nil {
		t.Fatal(err)
	}
	r.app.writeGuard = g
	r.app.emitHook = func(string, any) {}
	refused := func(err error) bool { return errors.Is(err, engine.ErrWriteRefused) }

	if _, err := r.app.CreateFolder(tProfile, bucket, tPrefix, "outside"); !refused(err) {
		t.Fatalf("folder outside: %v", err)
	}
	if _, ok := r.server.Object(bucket, tPrefix+"outside/"); ok {
		t.Fatal("the folder was created")
	}
	if key, err := r.app.CreateFolder(tProfile, bucket, inside, "reel"); err != nil || key != inside+"reel/" {
		t.Fatalf("folder inside: %q %v", key, err)
	}

	now := time.Now()
	for _, k := range []string{tPrefix + "out.bin", inside + "in.bin"} {
		if err := r.server.PutListed(bucket, k, 100, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.app.SaveObjectTags(tProfile, bucket, tPrefix+"out.bin", nil, []objects.Tag{{Key: "a", Value: "b"}}); !refused(err) {
		t.Fatalf("tags outside: %v", err)
	}
	if _, err := r.app.SaveObjectTags(tProfile, bucket, inside+"in.bin", nil, []objects.Tag{{Key: "a", Value: "b"}}); err != nil {
		t.Fatalf("tags inside: %v", err)
	}

	// The top listing holds inside/ and out.bin: selecting both is refused.
	top := r.openListing(tPrefix)
	sel := ObjectSelection{ProfileID: tProfile, Bucket: bucket, ListingID: top.ID, Field: "name", Version: top.Version, Ranges: [][2]int{{0, 1}}}
	if _, err := r.app.PrepareDelete(sel); !refused(err) || !strings.Contains(err.Error(), "out.bin") {
		t.Fatalf("delete across the prefix: %v", err)
	}
	if r.server.ObjectCount(bucket) != 3 {
		t.Fatal("something was deleted")
	}
	// Selecting only inside/ deletes what is below it.
	sel.Ranges = [][2]int{{0, 0}}
	p, err := r.app.PrepareDelete(sel)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.app.RunDelete(p.ID); err != nil || res.Deleted != 2 {
		t.Fatalf("delete inside: %+v %v", res, err)
	}
}
