package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"s4browser/internal/fakes3"
	"s4browser/internal/queue"
)

// A drop asks for confirmation only when it would replace something: an object
// with the same key, or any object under a dropped folder's prefix.
func TestUploadConflicts(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.withBrowse()
	now := time.Now()
	for _, k := range []string{"taken.bin", "dir/inside.bin", "other/x.bin", "fresh.bin.old"} {
		if err := r.server.PutListed(fakes3.DefaultBucket, tPrefix+k, 10, now); err != nil {
			t.Fatal(err)
		}
	}
	local := t.TempDir()
	mk := func(name string, dir bool) string {
		p := filepath.Join(local, name)
		var err error
		if dir {
			err = os.Mkdir(p, 0o755)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	taken, fresh := mk("taken.bin", false), mk("fresh.bin", false) // "fresh.bin.old" must not count
	dir, newDir := mk("dir", true), mk("newdir", true)

	check := func(paths ...string) []string {
		got, err := r.app.UploadConflicts(queue.UploadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Prefix: tPrefix, Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := check(fresh, newDir); len(got) != 0 {
		t.Fatalf("nothing exists, got %v", got)
	}
	if got := check(taken, fresh, dir, newDir, filepath.Join(local, "gone.bin"), "relative.bin"); !reflect.DeepEqual(got, []string{"taken.bin", "dir"}) {
		t.Fatalf("conflicts = %v", got)
	}
}
