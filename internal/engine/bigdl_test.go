package engine_test

import (
	"os"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
)

// TestSimulated2TiBDownload is the stage's main criterion: a 2 TiB download
// through the whole engine, with every byte verified against the synthetic source
// by the sink, written exactly once, and the heap bounded by the design's formula.
//
// The sink stores nothing and reports a 3 GiB MaxLead, which is smaller than the
// spread of 16 parts of 210 MiB (3.4 GiB). The lead gate, parking and preemption are
// therefore part of the run. An ordinary -race run moves 32 GiB, and
// S4_RACE_FULL_2TIB=1 with -timeout 0 moves the full 2 TiB.
func TestSimulated2TiBDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 2 TiB through the pattern check")
	}
	size := twoTiB
	if raceEnabled && os.Getenv(fullRaceEnv) != "1" {
		size = raceSmokeSize
		t.Logf("race detector on: downloading %d GiB. Set %s=1 and -timeout 0 for the full 2 TiB", size/gib, fullRaceEnv)
	}

	sink := fakes3.NewSink(func(string) uint64 { return testSeed })
	sink.MaxLead = 3 * gib
	h := newHarness(t, options{discardProgress: true, sink: sink, settings: func(s *engine.Settings) { s.PartSize = 0 }})
	h.putObject(dlKey, size)
	plan, err := engine.PlanParts(size, engine.DefaultPartSize)
	if err != nil {
		t.Fatal(err)
	}

	checkMemory := startMemoryCheck(t, engine.DefaultSettings(), 1)
	start := time.Now()
	id := h.download("big/movie.bin")
	job := h.waitBig(id)
	elapsed := time.Since(start)
	checkMemory()
	stats := h.engine.(engine.StatsProvider).DownloadStats()
	t.Logf("downloaded %d GiB in %d parts of %d MiB in %v (%.1f GiB/s), %d parks (longest %v), %d preemptions",
		size/gib, plan.PartCount, plan.PartSize/mib, elapsed.Round(time.Second), float64(size)/float64(gib)/elapsed.Seconds(),
		stats.Parks, stats.ParkedMax.Round(time.Millisecond), stats.Preemptions)

	if job.DoneBytes != size || job.TotalBytes != size || job.PartsDone != plan.PartCount || job.Retries != 0 {
		t.Fatalf("final snapshot: %+v", job)
	}
	// The sink checked every byte against the pattern. Each was written once, so
	// nothing was fetched twice, and the commit proves the whole range arrived.
	if total, distinct := sink.BytesWritten("big/movie.bin"); total != size || distinct != size {
		t.Fatalf("%d bytes written, %d distinct, want %d of each", total, distinct, size)
	}
	if !sink.Committed("big/movie.bin") {
		t.Fatal("not committed")
	}
	h.assertClean()
}
