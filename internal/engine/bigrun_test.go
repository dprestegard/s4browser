package engine_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
)

const (
	twoTiB = int64(2) << 40
	// raceSmokeSize is what an ordinary -race run uploads.
	raceSmokeSize = 32 * gib
	// fullRaceEnv makes a -race run upload the full 2 TiB. That run takes far
	// longer than the default test timeout, so pass -timeout 0.
	fullRaceEnv = "S4_RACE_FULL_2TIB"
)

// TestSimulated2TiBUpload is the stage's main criterion: a 2 TiB upload through
// the whole engine, with every part sent exactly once and all content verified.
func TestSimulated2TiBUpload(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 2 TiB through the CRC loop")
	}
	size := twoTiB
	if raceEnabled && os.Getenv(fullRaceEnv) != "1" {
		size = raceSmokeSize
		t.Logf("race detector on: uploading %d GiB. Set %s=1 and -timeout 0 for the full 2 TiB", size/gib, fullRaceEnv)
	}

	h := newHarness(t, options{discardProgress: true, settings: func(s *engine.Settings) { s.PartSize = 0 }})
	h.source.Set("big", fakes3.SyntheticFile{Size: size, Seed: testSeed})
	plan, err := engine.PlanParts(size, engine.DefaultPartSize)
	if err != nil {
		t.Fatal(err)
	}

	checkMemory := startMemoryCheck(t, engine.DefaultSettings(), 1)
	start := time.Now()
	id := h.submitAs("big", testRef)
	job := h.waitBig(id)
	elapsed := time.Since(start)
	checkMemory()
	t.Logf("uploaded %d GiB in %d parts of %d MiB in %v (%.1f GiB/s)",
		size/gib, plan.PartCount, plan.PartSize/mib, elapsed.Round(time.Second), float64(size)/float64(gib)/elapsed.Seconds())

	if job.DoneBytes != size || job.PartsDone != plan.PartCount || job.Retries != 0 {
		t.Fatalf("final snapshot: %+v", job)
	}
	for part := 1; part <= plan.PartCount; part++ {
		if n := h.attempts(int32(part)); n != 1 {
			t.Fatalf("part %d was sent %d times", part, n)
		}
	}
	object, ok := h.server.Object(fakes3.DefaultBucket, testKey)
	if !ok || object.Size != size || !strings.HasSuffix(object.CRC32, "-"+itoa(plan.PartCount)) {
		t.Fatalf("object: %+v", object.CRC32)
	}
	// Recompute every part's CRC32 from the synthetic pattern.
	verifyStart := time.Now()
	if err := h.server.VerifyObject(fakes3.DefaultBucket, testKey, testSeed, 1); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified all %d parts in %v", plan.PartCount, time.Since(verifyStart).Round(time.Second))
	h.assertClean()
}

// waitBig polls slowly, because a long run must not spend its time on snapshots.
func (h *harness) waitBig(id engine.JobID) engine.JobSnapshot {
	h.t.Helper()
	for {
		job := h.snapshot(id)
		if job.State.IsTerminal() {
			if job.State != engine.JobCompleted {
				h.t.Fatalf("job ended %s: %+v", job.State, job.Err)
			}
			return job
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}
