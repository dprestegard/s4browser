package browse

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// TestHundredThousandObjects is the stage 1.2 done criterion: 100,000 objects
// under one prefix are listed through the real SDK over HTTPS, then sorted by
// every column in both directions and paged through in windows, all in Go.
func TestHundredThousandObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("lists 100,000 objects")
	}
	const objects, folders = 100_000, 100
	e := newEnv(t, envOpts{})
	putMany(t, e.fake, "west-bucket", "big/", objects)
	for i := range folders {
		if err := e.fake.PutListed("west-bucket", fmt.Sprintf("big/folder-%03d/inside.bin", i), 1, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	start := time.Now()
	snap := e.open("west-bucket", "big/")
	snap = e.wait(snap.ID)
	listTime := time.Since(start)
	if snap.State != StateDone || snap.Objects != objects || snap.Folders != folders || snap.Truncated {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Pages != (objects+folders+999)/1000 {
		t.Errorf("%d pages", snap.Pages)
	}

	specs := []SortSpec{}
	for _, f := range []SortField{SortName, SortSize, SortModified, SortClass} {
		specs = append(specs, SortSpec{f, false}, SortSpec{f, true})
	}
	var worstSort, worstWindow time.Duration
	for _, spec := range specs {
		// The first window of an order pays for the sort.
		t0 := time.Now()
		if _, err := e.svc.Window(snap.ID, spec, 0, 100); err != nil {
			t.Fatal(err)
		}
		worstSort = max(worstSort, time.Since(t0))
		// Later windows anywhere in the listing reuse it.
		for _, offset := range []int{0, 50_000, 99_950} {
			t0 = time.Now()
			w, err := e.svc.Window(snap.ID, spec, offset, 100)
			if err != nil {
				t.Fatal(err)
			}
			worstWindow = max(worstWindow, time.Since(t0))
			if len(w.Rows) != min(100, objects+folders-offset) {
				t.Fatalf("%v at %d: %d rows", spec, offset, len(w.Rows))
			}
		}
		rows := e.all(snap.ID, spec)
		checkOrder(t, rows, spec, objects+folders)
		for i := range folders {
			if !rows[i].Folder {
				t.Fatalf("%v: row %d is not a folder", spec, i)
			}
		}
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	held := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	t.Logf("listed %d rows in %d pages in %v (%.0f rows/s)", snap.Count, snap.Pages, listTime.Round(time.Millisecond), float64(snap.Count)/listTime.Seconds())
	t.Logf("slowest first window (full sort of %d rows): %v", snap.Count, worstSort.Round(time.Microsecond))
	t.Logf("slowest later window of 100 rows: %v", worstWindow.Round(time.Microsecond))
	t.Logf("heap held by the listing with 8 sort orders: %.1f MiB", float64(held)/(1<<20))

	if raceEnabled {
		return // the race detector slows everything by 5 to 20 times
	}
	if worstSort > 500*time.Millisecond {
		t.Errorf("sorting 100,000 rows took %v, want under 500 ms", worstSort)
	}
	if worstWindow > 20*time.Millisecond {
		t.Errorf("a window took %v, want under 20 ms", worstWindow)
	}
	if held > 48<<20 {
		t.Errorf("listing holds %d MiB, want under 48 MiB", held>>20)
	}
}
