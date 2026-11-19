package faultsuite_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
)

// TestUploadFaultsOverHTTPS: network drops, 503 storms, throttling and stalls
// mid-transfer, through the real adapter and transport. Every upload completes,
// no finished part is sent again, and nothing is left behind.
func TestUploadFaultsOverHTTPS(t *testing.T) {
	cases := []struct {
		name  string
		rules func(k string) []fakes3.Rule
	}{
		{"503 storm on every part", func(k string) []fakes3.Rule {
			return []fakes3.Rule{{Op: fakes3.OpUploadPart, Key: k, FromAttempt: 1, Times: 40, Fault: fakes3.ServiceUnavailable()}}
		}},
		{"connection reset mid-body", func(k string) []fakes3.Rule {
			return []fakes3.Rule{
				{Op: fakes3.OpUploadPart, Key: k, Part: 3, Attempt: 1, Fault: fakes3.ResetAt(1 * mib)},
				{Op: fakes3.OpUploadPart, Key: k, Part: 3, Attempt: 2, Fault: fakes3.ResetAt(4 * mib)},
				{Op: fakes3.OpUploadPart, Key: k, Part: 9, Attempt: 1, Fault: fakes3.LostResponse()},
			}
		}},
		{"SlowDown with Retry-After", func(k string) []fakes3.Rule {
			return []fakes3.Rule{{Op: fakes3.OpUploadPart, Key: k, Part: 4, Attempt: 1, Fault: fakes3.SlowDown().WithRetryAfter(time.Second)}}
		}},
		{"stalled connection", func(k string) []fakes3.Rule {
			return []fakes3.Rule{{Op: fakes3.OpUploadPart, Key: k, Part: 2, Attempt: 1, Fault: fakes3.StallAt(1*mib, 0)}}
		}},
		{"internal errors on control requests", func(k string) []fakes3.Rule {
			return []fakes3.Rule{
				{Op: fakes3.OpCreateUpload, Key: k, Attempt: 1, Fault: fakes3.InternalError()},
				{Op: fakes3.OpComplete, Key: k, Attempt: 1, Fault: fakes3.InternalError().ErrorIn200()},
			}
		}},
		{"lost Complete response", func(k string) []fakes3.Rule {
			return []fakes3.Rule{{Op: fakes3.OpComplete, Key: k, Attempt: 1, Fault: fakes3.LostResponse()}}
		}},
	}
	server, guard := newServer(t)
	app := launch(t, server, guard, t.TempDir(), rigOptions{timeouts: func(to *engine.Timeouts) {
		to.Stall, to.Response = 2*time.Second, 3*time.Second
	}})
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := key("faults/" + string(rune('a'+i)) + ".bin")
			for _, rule := range tc.rules(k) {
				server.AddRule(rule)
			}
			id := app.upload(k)
			job := app.waitState(id, engine.JobCompleted)
			if job.Err != nil || job.Warning != "" {
				t.Fatalf("%+v", job)
			}
			assertVerified(t, server, k)
			assertEachPartStoredOnce(t, server, k)
			app.assertClean()
		})
	}
}

// TestDownloadFaultsOverHTTPS: the same for ranged GETs, ending in a verified
// object with every byte written exactly once.
func TestDownloadFaultsOverHTTPS(t *testing.T) {
	server, guard := newServer(t)
	k := key("download.bin")
	if err := server.PutSynthetic(fakes3.DefaultBucket, k, seed, objSize); err != nil {
		t.Fatal(err)
	}
	at := func(part int64) *int64 { offset := (part - 1) * partSize; return &offset }
	for _, rule := range []fakes3.Rule{
		// GET attempts are counted per key, not per range, so Times limits each rule.
		{Op: fakes3.OpGet, Key: k, RangeStart: at(2), Times: 2, Fault: fakes3.ServiceUnavailable()},
		{Op: fakes3.OpGet, Key: k, RangeStart: at(5), Times: 1, Fault: fakes3.Fault{Kind: fakes3.FaultReset, AtByte: 2 * mib, AfterHeaders: true}},
		{Op: fakes3.OpGet, Key: k, RangeStart: at(7), Times: 1, Fault: fakes3.TruncateAt(3 * mib)},
		{Op: fakes3.OpGet, Key: k, RangeStart: at(11), Times: 1, Fault: fakes3.Fault{Kind: fakes3.FaultStall, AtByte: mib, AfterHeaders: true}},
		{Op: fakes3.OpHead, Key: k, Attempt: 1, Fault: fakes3.ServiceUnavailable()},
	} {
		server.AddRule(rule)
	}
	app := launch(t, server, guard, t.TempDir(), rigOptions{timeouts: func(to *engine.Timeouts) {
		to.Stall, to.Response = 2*time.Second, 3*time.Second
	}})
	id := app.download(k, `C:\downloads\movie.bin`)
	job := app.waitState(id, engine.JobCompleted)
	if job.Retries < 5 {
		t.Fatalf("only %d retries: the faults did not fire", job.Retries)
	}
	total, distinct := app.sink.BytesWritten(`C:\downloads\movie.bin`)
	if total != objSize || distinct != objSize {
		t.Fatalf("wrote %d bytes, %d distinct, want %d once each", total, distinct, objSize)
	}
	if !app.sink.Committed(`C:\downloads\movie.bin`) {
		t.Fatal("the download was not committed")
	}
	app.assertClean()
}

// TestPauseAndResumeOverHTTPS pauses mid-transfer, checks that nothing moves
// while paused and that resuming sends only the parts that were not finished.
func TestPauseAndResumeOverHTTPS(t *testing.T) {
	server, guard := newServer(t)
	k := key("paused.bin")
	server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: k, Fault: fakes3.SlowBody(40 << 20)})
	server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: k, Part: 7, Attempt: 1, Fault: fakes3.ServiceUnavailable().WithRetryAfter(time.Second)})
	app := launch(t, server, guard, t.TempDir(), rigOptions{settings: func(s *engine.Settings) { s.MaxPartsPerFile = 2 }})
	id := app.upload(k)
	app.waitFor(id, "four finished parts", func(job engine.JobSnapshot) bool { return job.PartsDone >= 4 })
	if err := app.eng.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	paused := app.waitState(id, engine.JobPaused)
	if paused.PartsDone == parts {
		t.Fatal("the job finished before it could be paused")
	}
	if uploads := server.ActiveUploads(fakes3.DefaultBucket); len(uploads) != 1 {
		t.Fatalf("a paused upload must stay on S3: %+v", uploads)
	}
	// Count requests started, not finished: a cancelled attempt's server side
	// can still be finishing when the job reports paused.
	before := startedParts(server, k)
	time.Sleep(300 * time.Millisecond)
	if after := startedParts(server, k); after != before {
		t.Fatalf("%d part requests started while paused", after-before)
	}

	storedWhilePaused := storedParts(server, k)
	if err := app.eng.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	app.waitState(id, engine.JobCompleted)
	assertVerified(t, server, k)
	assertStoredOnceExceptCutOff(t, server, k, storedWhilePaused, paused.PartsDone)
	app.assertClean()
}

// storedParts returns how often S3 stored each part.
func storedParts(server *fakes3.Server, k string) map[int32]int {
	stores := make(map[int32]int)
	for _, rec := range server.Requests() {
		if rec.Op == fakes3.OpUploadPart && rec.Key == k && rec.Status == http.StatusOK {
			stores[rec.Part]++
		}
	}
	return stores
}

// assertStoredOnceExceptCutOff is assertEachPartStoredOnce for a paused job. A
// part in flight when the pause began can be stored by S3 just as its attempt
// is cancelled. Its answer is lost, so resuming sends it again. Exactly the
// parts S3 had stored beyond those the engine had recorded as done may repeat,
// once each, and no other part.
func assertStoredOnceExceptCutOff(t *testing.T, server *fakes3.Server, k string, before map[int32]int, doneAtPause int) {
	t.Helper()
	cutOff := len(before) - doneAtPause
	repeated := 0
	for part, n := range storedParts(server, k) {
		switch {
		case n == 2 && before[part] == 1:
			repeated++
		case n != 1:
			t.Errorf("part %d was stored %d times (%d before resuming)", part, n, before[part])
		}
	}
	if repeated > cutOff {
		t.Errorf("%d parts were sent again, but only %d were cut off by the pause", repeated, cutOff)
	}
	if got := len(storedParts(server, k)); got != parts {
		t.Errorf("%d parts stored, want %d", got, parts)
	}
}

// TestPauseDuringCredentialWaitOverHTTPS: pausing while credentials are expired
// keeps the upload. Resuming with credentials still expired waits again, and
// the job completes once they return.
func TestPauseDuringCredentialWaitOverHTTPS(t *testing.T) {
	server, guard := newServer(t)
	k := key("paused-waiting.bin")
	prober := newGatedProber()
	expired := server.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Key: k, Part: 3, Fault: fakes3.ExpiredToken()})
	app := launch(t, server, guard, t.TempDir(), rigOptions{prober: prober, settings: func(s *engine.Settings) { s.MaxPartsPerFile = 4 }})
	id := app.upload(k)
	app.waitState(id, engine.JobCredentialWait)

	if err := app.eng.Pause(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	paused := app.waitState(id, engine.JobPaused)
	storedWhilePaused := storedParts(server, k)
	if err := app.eng.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	app.waitState(id, engine.JobCredentialWait)

	server.RemoveRule(expired)
	close(prober.open)
	job := app.waitState(id, engine.JobCompleted)
	if job.Retries != 0 {
		t.Fatalf("credential failures were charged as retries: %d", job.Retries)
	}
	assertVerified(t, server, k)
	assertStoredOnceExceptCutOff(t, server, k, storedWhilePaused, paused.PartsDone)
	app.assertClean()
}

// TestCancelWhileCredentialsExpiredThenRelaunch: the abort itself fails on the
// expired credentials, so the job ends with a warning and its entry stays. The
// next launch, with working credentials, aborts the upload.
func TestCancelWhileCredentialsExpiredThenRelaunch(t *testing.T) {
	server, guard := newServer(t)
	k := key("cancelled-waiting.bin")
	dir := t.TempDir()
	app := launch(t, server, guard, dir, rigOptions{prober: newGatedProber(), settings: func(s *engine.Settings) { s.MaxPartsPerFile = 2 }})
	id := app.upload(k)
	app.waitFor(id, "two finished parts", func(job engine.JobSnapshot) bool { return job.PartsDone >= 2 })
	server.AddRule(fakes3.Rule{Key: k, Fault: fakes3.ExpiredToken()})
	app.waitState(id, engine.JobCredentialWait)

	if err := app.eng.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	job := app.waitState(id, engine.JobCancelled)
	if job.Warning == "" {
		t.Fatal("a cancel whose abort failed must warn")
	}
	if uploads := server.ActiveUploads(fakes3.DefaultBucket); len(uploads) != 1 {
		t.Fatalf("uploads %+v", uploads)
	}
	if entries := app.journal.List(); len(entries) != 1 || entries[0].Phase != journal.PhaseActive {
		t.Fatalf("the entry must stay for next launch: %+v", entries)
	}
	app.shutdown()

	server.ClearRules()
	next := launch(t, server, guard, dir, rigOptions{})
	if report := next.recover(); report.UploadsAborted != 1 || report.Deferred != 0 {
		t.Fatalf("report %+v", report)
	}
	next.assertClean()
}

// TestCloseKeepsUnknownOutcomeForNextLaunch: closing the app while Complete is
// in flight leaves the outcome unknown. The next launch finds the object
// complete, the abort reports the upload gone, and the entry is removed.
func TestCloseKeepsUnknownOutcomeForNextLaunch(t *testing.T) {
	server, guard := newServer(t)
	k := key("closed-completing.bin")
	dir := t.TempDir()
	// Complete succeeds on S3, but the answer never comes back.
	server.AddRule(fakes3.Rule{Op: fakes3.OpComplete, Key: k, Fault: fakes3.Fault{Kind: fakes3.FaultStall, After: true}})
	app := launch(t, server, guard, dir, rigOptions{})
	id := app.upload(k)
	app.waitFor(id, "finalizing", func(job engine.JobSnapshot) bool { return job.PartsDone == parts && job.Phase == engine.PhaseFinalizing })
	waitForRequest(t, server, fakes3.OpComplete, k)
	app.shutdown()
	if entries := openAndList(t, dir); len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}

	server.ClearRules()
	next := launch(t, server, guard, dir, rigOptions{})
	if report := next.recover(); report.UploadsAborted != 0 || report.Deferred != 0 || len(report.Warnings) != 0 {
		t.Fatalf("report %+v", report)
	}
	next.assertClean()
	assertVerified(t, server, k)
}

func waitForRequest(t *testing.T, server *fakes3.Server, op fakes3.Op, k string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for server.Attempts(op, fakes3.DefaultBucket, k, 0) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no %s request", op)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func openAndList(t *testing.T, dir string) []journal.Entry {
	t.Helper()
	store := openJournal(t, dir)
	defer store.Close()
	return store.List()
}

func startedParts(server *fakes3.Server, k string) int {
	n := 0
	for part := int32(1); part <= parts; part++ {
		n += server.Attempts(fakes3.OpUploadPart, fakes3.DefaultBucket, k, part)
	}
	return n
}
