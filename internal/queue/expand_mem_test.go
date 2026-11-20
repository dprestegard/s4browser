//go:build !race

package queue

import (
	"context"
	"fmt"
	"io/fs"
	"runtime"
	"testing"

	"s4browser/internal/engine"
)

// The race detector multiplies memory use, so the measurements run without it.

func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// bigLister serves dirs folders of per files each, generated as they are read,
// plus links, so the refusal list overflows its cap.
type bigLister struct{ dirs, per int }

func (b bigLister) ReadDir(dir string, batch func([]fs.DirEntry) error) error {
	if dir == fakeRoot {
		var subs []fs.DirEntry
		for d := range b.dirs {
			subs = append(subs, ent{name: fmt.Sprintf("d%05d", d), mode: fs.ModeDir})
		}
		return batch(subs)
	}
	buf := make([]fs.DirEntry, 0, dirBatch)
	for i := range b.per {
		mode := fs.FileMode(0)
		if i%100 == 0 {
			mode = fs.ModeSymlink
		}
		buf = append(buf, ent{name: fmt.Sprintf("file%07d.bin", i), mode: mode})
		if len(buf) == cap(buf) {
			if err := batch(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	if len(buf) > 0 {
		return batch(buf)
	}
	return nil
}

// TestLargeTreeUsesBoundedMemory expands a million files, in 1,000 folders, and
// checks that the live heap does not follow the file count. A version that
// kept every file or key (the mutation tried in stage 2.3) grows by 100 MB or more.
func TestLargeTreeUsesBoundedMemory(t *testing.T) {
	noLeak(t)
	const dirs, per = 1000, 1000
	before := liveHeap()
	var peak uint64
	n := 0
	submit := func(local, key string) (engine.JobID, error) {
		n++
		if n%100000 == 0 {
			if h := liveHeap(); h > peak {
				peak = h
			}
		}
		return "id", nil
	}
	item := &SubmitItem{Kind: KindFolder}
	w := &walker{ctx: context.Background(), item: item, fsys: bigLister{dirs, per}, root: fakeRoot, base: "up/tree/", submit: submit}
	if err := w.walk(""); err != nil {
		t.Fatal(err)
	}
	wantQueued := dirs * (per - per/100)
	if item.Queued != wantQueued || item.RefusedTotal != dirs*per/100 || item.Examined != dirs*per {
		t.Fatalf("queued %d refused %d examined %d", item.Queued, item.RefusedTotal, item.Examined)
	}
	if len(item.Refused) != MaxListedRefusals || len(item.JobIDs) != MaxListedJobs {
		t.Fatalf("lists: %d, %d", len(item.Refused), len(item.JobIDs))
	}
	growth := int64(peak) - int64(before)
	t.Logf("%d files expanded, live heap growth at the peak: %d KiB", dirs*per, growth/1024)
	if growth > 8<<20 {
		t.Fatalf("live heap grew by %d MiB while expanding %d files", growth>>20, dirs*per)
	}
}

// TestLargeListingMemoryPerObject: a prefix of 200,000 objects costs a few tens
// of bytes each (the collision hashes) and nothing else.
func TestLargeListingMemoryPerObject(t *testing.T) {
	noLeak(t)
	const dirs, per = 200, 1000
	p, _ := destPlacer(t)
	list := func(ctx context.Context, prefix string, fn func(string, int64) error) error {
		for d := range dirs {
			for i := range per {
				key := fmt.Sprintf("%sd%04d/file%06d.bin", prefix, d, i)
				if i%100 == 0 {
					key = fmt.Sprintf("%sd%04d/bad:%06d.bin", prefix, d, i) // Refused, so the refusal list overflows.
				}
				if err := fn(key, 1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	before := liveHeap()
	var peak uint64
	n := 0
	submit := func(key, dest string) (engine.JobID, error) {
		if n++; n%50000 == 0 {
			if h := liveHeap(); h > peak {
				peak = h
			}
		}
		return "id", nil
	}
	item := &SubmitItem{Kind: KindPrefix}
	if err := expandPrefix(context.Background(), item, p, "big/", "big", list, submit); err != nil {
		t.Fatal(err)
	}
	if item.Queued != dirs*(per-per/100) || item.RefusedTotal != dirs*per/100 {
		t.Fatalf("queued %d refused %d", item.Queued, item.RefusedTotal)
	}
	if len(item.Refused) != MaxListedRefusals || len(item.JobIDs) != MaxListedJobs {
		t.Fatalf("lists: %d, %d", len(item.Refused), len(item.JobIDs))
	}
	growth := int64(peak) - int64(before)
	per1 := growth / int64(item.Queued)
	t.Logf("live heap growth at the peak: %d KiB, %d bytes per queued object", growth/1024, per1)
	if per1 > 100 {
		t.Fatalf("%d bytes per object: the listing is being kept", per1)
	}
}
