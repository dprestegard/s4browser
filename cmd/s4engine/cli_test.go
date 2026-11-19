package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
)

const mib = int64(1) << 20

// harness runs the CLI in process against a fake S3 that tests can inspect.
type harness struct {
	t      *testing.T
	server *fakes3.Server
	dir    string
}

func newHarness(t *testing.T, guardRunID string) *harness {
	t.Helper()
	var guard engine.WriteGuard
	if guardRunID != "" {
		g, err := engine.NewTestPrefixGuard(guardRunID)
		if err != nil {
			t.Fatal(err)
		}
		guard = g
	}
	server, err := fakes3.New(fakes3.Options{Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return &harness{t: t, server: server, dir: t.TempDir()}
}

type output struct{ stdout, stderr bytes.Buffer }

func (h *harness) run(ctx context.Context, args ...string) (int, *output) {
	h.t.Helper()
	var out output
	code := run(ctx, args, env{stdout: &out.stdout, stderr: &out.stderr, fake: h.server})
	return code, &out
}

func (h *harness) path(name string) string { return filepath.Join(h.dir, name) }

func (h *harness) generate(name string, size int64, seed uint64) string {
	h.t.Helper()
	p := h.path(name)
	code, out := h.run(context.Background(), "generate", "--out", p, "--size", fmt.Sprint(size), "--seed", fmt.Sprint(seed))
	if code != exitOK {
		h.t.Fatalf("generate: exit %d\n%s", code, out.stderr.String())
	}
	return p
}

func readResult(t *testing.T, path string) result {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r result
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("result file is not valid JSON: %v", err)
	}
	return r
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	h := newHarness(t, "rt1")
	const size = 200*mib + 12345 // not a multiple of the part size
	src := h.generate("source.bin", size, 42)

	up := h.path("up.json")
	code, out := h.run(context.Background(), "upload", "--fake", "--run-id", "rt1", "--part-size", "16MiB", "--seed", "42", "--result", up, src)
	if code != exitOK {
		t.Fatalf("upload: exit %d\n%s%s", code, out.stdout.String(), out.stderr.String())
	}
	key := "s4browser-test/rt1/source.bin"
	if err := h.server.VerifyObject(fakes3.DefaultBucket, key, 42, 1); err != nil {
		t.Fatalf("the fake did not receive the file's bytes: %v", err)
	}
	if r := readResult(t, up); r.FakeVerified == nil || !*r.FakeVerified {
		t.Fatalf("upload result: fakeVerified = %v", r.FakeVerified)
	}
	// Every part of the 16 MiB plan was sent once.
	for part := int32(1); part <= 13; part++ {
		if n := h.server.Attempts(fakes3.OpUploadPart, fakes3.DefaultBucket, key, part); n != 1 {
			t.Fatalf("part %d sent %d times", part, n)
		}
	}

	dest := h.path("out")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	down := h.path("down.json")
	code, out = h.run(context.Background(), "download", "--fake", "--run-id", "rt1", "--dest-dir", dest, "--part-size", "16MiB", "--result", down, key)
	if code != exitOK {
		t.Fatalf("download: exit %d\n%s%s", code, out.stdout.String(), out.stderr.String())
	}
	got := filepath.Join(dest, "source.bin")
	a, na, err := hashFile(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	b, nb, err := hashFile(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || na != nb || na != size {
		t.Fatalf("round trip changed the file: %s (%d) != %s (%d)", a, na, b, nb)
	}
	if code, out := h.run(context.Background(), "verify", src, got); code != exitOK || !strings.Contains(out.stdout.String(), "MATCH") {
		t.Fatalf("verify: exit %d\n%s", code, out.stdout.String())
	}
	if r := readResult(t, down); r.Outcome != "completed" || r.Bytes != size || r.Settings.WriteStrategy == "" {
		t.Fatalf("download result: %+v", r)
	}
	// No temp file is left beside the download.
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Fatalf("download directory holds %d entries", len(entries))
	}
}

func TestDownloadFromGeneratedFakeObject(t *testing.T) {
	// The standalone path: a fresh fake, an object made from a seed, one run.
	dir := t.TempDir()
	var out output
	code := run(context.Background(), []string{
		"download", "--fake", "--run-id", "solo", "--seed", "9", "--fake-size", "70MiB", "--dest-dir", dir,
		"--result", filepath.Join(dir, "r.json"), "s4browser-test/solo/obj.bin",
	}, env{stdout: &out.stdout, stderr: &out.stderr})
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out.stderr.String())
	}
	ref := filepath.Join(t.TempDir(), "ref.bin")
	if code, _ := (&harness{t: t, dir: filepath.Dir(ref)}).run(context.Background(), "generate", "--out", ref, "--size", "70MiB", "--seed", "9"); code != exitOK {
		t.Fatal("generate failed")
	}
	if code, o := (&harness{t: t}).run(context.Background(), "verify", ref, filepath.Join(dir, "obj.bin")); code != exitOK {
		t.Fatalf("downloaded bytes differ from the seed's pattern\n%s", o.stdout.String())
	}
}

func TestResultFileMatchesKnownTransfer(t *testing.T) {
	h := newHarness(t, "res1")
	const size = 100 * mib
	src := h.generate("known.bin", size, 5)
	resPath := h.path("res.json")
	code, out := h.run(context.Background(), "upload", "--fake", "--run-id", "res1", "--part-size", "10MiB",
		"--parallel-files", "2", "--parallel-parts", "3", "--total-parts", "6", "--bandwidth", "400MiB",
		"--result", resPath, src)
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out.stderr.String())
	}
	r := readResult(t, resPath)
	if r.Command != "upload" || r.Outcome != "completed" || r.Bytes != size {
		t.Fatalf("outcome: %+v", r)
	}
	if len(r.Files) != 1 || r.Files[0].Bytes != size || r.Files[0].Parts != 10 || r.Files[0].State != "completed" || r.Files[0].Key != "s4browser-test/res1/known.bin" {
		t.Fatalf("files: %+v", r.Files)
	}
	if r.DurationS <= 0 || r.DurationS > 60 {
		t.Fatalf("duration %v", r.DurationS)
	}
	if want := float64(size) / r.DurationS; r.Throughput < want*0.999 || r.Throughput > want*1.001 {
		t.Fatalf("throughput %v, want %v", r.Throughput, want)
	}
	// 100 MiB at a 400 MiB/s cap takes at least 0.2 s, and a run this short has
	// neither a steady state nor a 10 s window.
	if r.DurationS < 0.2 || r.SteadyState != nil || r.Slowest10s != nil {
		t.Fatalf("duration %v, steady %v, slowest %v", r.DurationS, r.SteadyState, r.Slowest10s)
	}
	if r.PeakHeap == 0 || r.PeakWorkingSet == 0 || r.Commit == "" || r.GoVersion == "" {
		t.Fatalf("missing measurements: %+v", r)
	}
	want := settingsOut{ParallelFiles: 2, ParallelPartsPerFile: 3, TotalParallelParts: 6, PartSizeBytes: 10 * mib, BandwidthBytesPerSec: 400 * mib}
	if r.Settings != want {
		t.Fatalf("settings %+v, want %+v", r.Settings, want)
	}
	if r.Target.Kind != "fake" || r.Target.Prefix != "s4browser-test/res1/" || r.Target.RunID != "res1" {
		t.Fatalf("target: %+v", r.Target)
	}
	// A result holds names, sizes and timings, never credentials.
	raw, _ := os.ReadFile(resPath)
	for _, secret := range []string{fakes3.AccessKeyID, fakes3.SecretAccessKey, "secret", "Authorization"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("result file contains %q", secret)
		}
	}
	if !strings.Contains(out.stderr.String(), "Gbps") || !strings.Contains(out.stdout.String(), "upload completed") {
		t.Fatalf("no live progress or summary:\n%s\n%s", out.stderr.String(), out.stdout.String())
	}
}

func TestCancelAbortsMultipartUpload(t *testing.T) {
	h := newHarness(t, "cx1")
	src := h.generate("big.bin", 120*mib, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resPath := h.path("cancel.json")
	done := make(chan int, 1)
	var out *output
	go func() {
		var code int
		code, out = h.run(ctx, "upload", "--fake", "--run-id", "cx1", "--part-size", "8MiB", "--bandwidth", "4MiB", "--result", resPath, src)
		done <- code
	}()
	// Wait until the multipart upload exists on the server, then press "Ctrl+C".
	deadline := time.Now().Add(20 * time.Second)
	for len(h.server.ActiveUploads(fakes3.DefaultBucket)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upload never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != exitInterrupted {
			t.Fatalf("exit %d, want %d\n%s", code, exitInterrupted, out.stderr.String())
		}
	case <-time.After(40 * time.Second):
		t.Fatal("cancel did not finish")
	}
	if n := len(h.server.ActiveUploads(fakes3.DefaultBucket)); n != 0 {
		t.Fatalf("%d multipart upload(s) left after cancel", n)
	}
	if _, ok := h.server.Object(fakes3.DefaultBucket, "s4browser-test/cx1/big.bin"); ok {
		t.Fatal("a cancelled upload left an object")
	}
	if r := readResult(t, resPath); r.Outcome != "cancelled" || r.Bytes != 0 {
		t.Fatalf("result: %+v", r)
	}
}

func TestTransferFailureExitCode(t *testing.T) {
	h := newHarness(t, "nf1")
	dir := t.TempDir()
	code, out := h.run(context.Background(), "download", "--fake", "--run-id", "nf1", "--dest-dir", dir, "--result", h.path("r.json"), "s4browser-test/nf1/missing.bin")
	if code != exitFailed {
		t.Fatalf("exit %d, want %d\n%s", code, exitFailed, out.stderr.String())
	}
	if r := readResult(t, h.path("r.json")); r.Outcome != "failed" || r.Files[0].Error == "" {
		t.Fatalf("result: %+v", r)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a failed download left %d file(s)", len(entries))
	}
}

func TestGuardRefusesDestinationOutsidePrefix(t *testing.T) {
	h := newHarness(t, "")
	src := h.generate("a.bin", 6*mib, 1)
	for _, key := range []string{
		"a.bin",
		"s4browser-test/other/a.bin",
		"s4browser-test/g1",
		"s4browser-test/g1/",
		"s4browser-test/g1/../other/a.bin",
		"s4browser-test/g1/x//a.bin",
		"s4browser-test/g1x/a.bin",
		"s4browser-test/g1/a\\b",
	} {
		before := len(h.server.Requests())
		code, out := h.run(context.Background(), "upload", "--fake", "--run-id", "g1", "--key", key, src)
		if code != exitUsage || !strings.Contains(out.stderr.String(), "refusing to start") {
			t.Errorf("key %q: exit %d, stderr %q", key, code, out.stderr.String())
		}
		if len(h.server.Requests()) != before {
			t.Errorf("key %q: a refused upload sent requests", key)
		}
	}
	// A download of a key outside the prefix is refused as well.
	code, _ := h.run(context.Background(), "download", "--fake", "--run-id", "g1", "--dest-dir", h.dir, "elsewhere/a.bin")
	if code != exitUsage {
		t.Errorf("download outside the prefix: exit %d", code)
	}
	if len(h.server.Requests()) != 0 {
		t.Errorf("requests were made: %d", len(h.server.Requests()))
	}
}

func TestGuardRefusalComesBeforeCredentials(t *testing.T) {
	// With a real target, the refusal must happen before the profile is even
	// loaded. The profile does not exist: loading it would be exit 1.
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	src := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out output
	e := env{stdout: &out.stdout, stderr: &out.stderr}
	args := func(more ...string) []string {
		return append([]string{"upload", "--profile", "nosuch", "--region", "us-east-1", "--bucket", "b"}, more...)
	}
	if code := run(context.Background(), args("--run-id", "r1", "--key", "prod/data/a.bin", src), e); code != exitUsage {
		t.Fatalf("key outside the prefix: exit %d\n%s", code, out.stderr.String())
	}
	// Control: a key inside the prefix does reach credential loading and fails there.
	out = output{}
	if code := run(context.Background(), args("--run-id", "r1", "--key", "s4browser-test/r1/a.bin", src), e); code != exitFailed {
		t.Fatalf("key inside the prefix: exit %d\n%s", code, out.stderr.String())
	}
	// Cleanup needs an explicit run ID, and the ID must be valid.
	for _, a := range [][]string{
		{"cleanup", "--profile", "nosuch", "--region", "us-east-1", "--bucket", "b"},
		{"cleanup", "--profile", "nosuch", "--region", "us-east-1", "--bucket", "b", "--run-id", "../x"},
		{"cleanup", "--profile", "nosuch", "--region", "us-east-1", "--bucket", "b", "--run-id", ""},
	} {
		if code := run(context.Background(), a, e); code != exitUsage {
			t.Fatalf("%v: exit %d", a, code)
		}
	}
}

func TestNoFlagDisablesTheGuard(t *testing.T) {
	// The guard is wired in newTarget and in the engine config without a switch.
	// This pins that the command line has no such flag.
	for _, cmd := range []string{"upload", "download", "cleanup"} {
		var out output
		run(context.Background(), []string{cmd, "-h"}, env{stdout: &out.stdout, stderr: &out.stderr})
		help := strings.ToLower(out.stderr.String())
		if help == "" {
			t.Fatalf("%s -h printed no flags", cmd)
		}
		for _, banned := range []string{"guard", "unsafe", "force", "no-prefix", "any-key", "disable"} {
			if strings.Contains(help, banned) {
				t.Errorf("%s help mentions %q:\n%s", cmd, banned, help)
			}
		}
	}
}

func TestCleanupStaysInsidePrefix(t *testing.T) {
	h := newHarness(t, "") // no server guard, so the test can plant objects anywhere
	const bucket = fakes3.DefaultBucket
	inside := []string{"s4browser-test/c1/a.bin", "s4browser-test/c1/sub/b.bin"}
	outside := []string{"s4browser-test/c10/a.bin", "s4browser-test/c1", "s4browser-test/c2/a.bin", "prod/keep.bin", "s4browser-test/c1x"}
	for _, key := range append(append([]string{}, inside...), outside...) {
		if err := h.server.PutSynthetic(bucket, key, 1, 1024); err != nil {
			t.Fatal(err)
		}
	}
	client := h.server.S3Client()
	var insideUploads, outsideUploads int
	for _, key := range append(append([]string{}, inside...), outside...) {
		if _, err := client.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key + ".mpu")}); err != nil {
			t.Fatal(err)
		}
	}
	insideUploads, outsideUploads = len(inside), len(outside)
	if got := len(h.server.ActiveUploads(bucket)); got != insideUploads+outsideUploads {
		t.Fatalf("setup: %d uploads", got)
	}

	// A dry run changes nothing.
	code, out := h.run(context.Background(), "cleanup", "--fake", "--run-id", "c1", "--dry-run", "--result", h.path("dry.json"))
	if code != exitOK {
		t.Fatalf("dry run: exit %d\n%s", code, out.stderr.String())
	}
	if got := len(h.server.ActiveUploads(bucket)); got != insideUploads+outsideUploads || h.server.ObjectCount(bucket) != len(inside)+len(outside) {
		t.Fatal("a dry run changed the bucket")
	}
	if r := readCleanup(t, h.path("dry.json")); !r.DryRun || r.UploadsFound != insideUploads || r.ObjectsFound != len(inside) || r.UploadsAborted != 0 {
		t.Fatalf("dry run result: %+v", r)
	}

	code, out = h.run(context.Background(), "cleanup", "--fake", "--run-id", "c1", "--result", h.path("c.json"))
	if code != exitOK {
		t.Fatalf("cleanup: exit %d\n%s", code, out.stderr.String())
	}
	if r := readCleanup(t, h.path("c.json")); r.UploadsAborted != insideUploads || r.ObjectsDeleted != len(inside) || len(r.Errors) != 0 {
		t.Fatalf("result: %+v", r)
	}
	for _, key := range inside {
		if _, ok := h.server.Object(bucket, key); ok {
			t.Errorf("%s was not deleted", key)
		}
	}
	for _, key := range outside {
		if _, ok := h.server.Object(bucket, key); !ok {
			t.Errorf("%s outside the prefix was deleted", key)
		}
	}
	if got := len(h.server.ActiveUploads(bucket)); got != outsideUploads {
		t.Errorf("%d uploads remain, want the %d outside the prefix", got, outsideUploads)
	}
	for _, u := range h.server.ActiveUploads(bucket) {
		if strings.HasPrefix(u.Key, "s4browser-test/c1/") {
			t.Errorf("upload %s inside the prefix survived", u.Key)
		}
	}
}

func readCleanup(t *testing.T, path string) cleanupResult {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r cleanupResult
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// recordingAdmin records what reaches the layer below the guard.
type recordingAdmin struct{ calls []string }

func (r *recordingAdmin) ListUploads(_ context.Context, _, prefix string) ([]uploadRef, error) {
	r.calls = append(r.calls, "list-uploads "+prefix)
	return nil, nil
}
func (r *recordingAdmin) AbortUpload(_ context.Context, _, key, _ string) error {
	r.calls = append(r.calls, "abort "+key)
	return nil
}
func (r *recordingAdmin) ListObjects(_ context.Context, _, prefix string) ([]string, error) {
	r.calls = append(r.calls, "list-objects "+prefix)
	return nil, nil
}
func (r *recordingAdmin) DeleteObject(_ context.Context, _, key string) error {
	r.calls = append(r.calls, "delete "+key)
	return nil
}

func TestGuardedAdminRefusesBeforeTheNetwork(t *testing.T) {
	guard, _ := engine.NewTestPrefixGuard("a1")
	rec := &recordingAdmin{}
	adm := guardedAdmin{rec, guard}
	ctx := context.Background()
	for _, key := range []string{"other/x", "s4browser-test/a10/x", "s4browser-test/a1/../x", "s4browser-test/a1/", "s4browser-test/a1"} {
		if err := adm.AbortUpload(ctx, "b", key, "id"); !errors.Is(err, engine.ErrWriteRefused) {
			t.Errorf("abort %q: %v", key, err)
		}
		if err := adm.DeleteObject(ctx, "b", key); !errors.Is(err, engine.ErrWriteRefused) {
			t.Errorf("delete %q: %v", key, err)
		}
	}
	for _, prefix := range []string{"", "s4browser-test/", "s4browser-test/a1", "s4browser-test/a10/", "other/"} {
		if _, err := adm.ListUploads(ctx, "b", prefix); !errors.Is(err, engine.ErrWriteRefused) {
			t.Errorf("list uploads %q: %v", prefix, err)
		}
		if _, err := adm.ListObjects(ctx, "b", prefix); !errors.Is(err, engine.ErrWriteRefused) {
			t.Errorf("list objects %q: %v", prefix, err)
		}
	}
	if len(rec.calls) != 0 {
		t.Fatalf("refused calls reached the store: %v", rec.calls)
	}
	if err := adm.DeleteObject(ctx, "b", "s4browser-test/a1/x"); err != nil || len(rec.calls) != 1 {
		t.Fatalf("an allowed delete: %v %v", err, rec.calls)
	}
}

func TestVerifyDetectsDifference(t *testing.T) {
	h := newHarness(t, "")
	a := h.generate("a.bin", 3*mib, 1)
	b := h.generate("b.bin", 3*mib, 2)
	if code, out := h.run(context.Background(), "verify", a, b); code != exitFailed || !strings.Contains(out.stdout.String(), "MISMATCH") {
		t.Fatalf("exit %d\n%s", code, out.stdout.String())
	}
	if code, _ := h.run(context.Background(), "verify", a); code != exitUsage {
		t.Fatalf("one file: exit %d", code)
	}
	if code, _ := h.run(context.Background(), "verify", a, h.path("missing.bin")); code != exitFailed {
		t.Fatalf("missing file: exit %d", code)
	}
}

func TestGenerateIsDeterministicAndNeverOverwrites(t *testing.T) {
	h := newHarness(t, "")
	a := h.generate("a.bin", 5*mib+7, 11)
	b := h.generate("b.bin", 5*mib+7, 11)
	c := h.generate("c.bin", 5*mib+7, 12)
	ha, _, _ := hashFile(context.Background(), a)
	hb, _, _ := hashFile(context.Background(), b)
	hc, _, _ := hashFile(context.Background(), c)
	if ha != hb || ha == hc {
		t.Fatal("same seed must give the same bytes and another seed other bytes")
	}
	if info, _ := os.Stat(a); info.Size() != 5*mib+7 {
		t.Fatalf("size %d", info.Size())
	}
	if code, _ := h.run(context.Background(), "generate", "--out", a, "--size", "1MiB"); code != exitUsage {
		t.Fatalf("overwrite: exit %d", code)
	}
}

func TestUsageErrors(t *testing.T) {
	h := newHarness(t, "")
	src := h.generate("a.bin", mib, 1)
	for _, args := range [][]string{
		nil,
		{"frobnicate"},
		{"upload"},
		{"upload", "--fake"},
		{"upload", "--bogus", src},
		{"upload", "--fake", "--profile", "p", src},
		{"upload", "--profile", "p", src},
		{"upload", "--fake", "--part-size", "1KiB", src},
		{"upload", "--fake", "--parallel-files", "1000", src},
		{"upload", "--fake", "--bandwidth", "fast", src},
		{"upload", "--fake", "--key", "s4browser-test/x/a", src, src},
		{"upload", "--fake", h.path("nope.bin")},
		{"download", "--fake", "s4browser-test/x/a"},
		{"download", "--fake", "--dest-dir", h.path("nope"), "s4browser-test/x/a"},
		{"download", "--fake", "--write-strategy", "bogus", "--dest-dir", h.dir, "--run-id", "x", "s4browser-test/x/a"},
		{"upload", "--fake", "--fake-size", "1MiB", src},
		{"generate", "--out", h.path("g.bin")},
		{"cleanup", "--fake", "extra"},
	} {
		if code, out := h.run(context.Background(), args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d\n%s", args, code, exitUsage, out.stderr.String())
		}
	}
}

func TestParseSizeAndRate(t *testing.T) {
	sizes := map[string]int64{"0": 0, "5368709120": 5 << 30, "64MiB": 64 * mib, "1.5GiB": 3 << 29, "10GB": 10e9, "2 TiB": 2 << 40, "5kib": 5120}
	for in, want := range sizes {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	rates := map[string]int64{"5Gbit": 625_000_000, "500Mbit": 62_500_000, "50MiB": 50 * mib, "625MB/s": 625_000_000, "8bit": 1}
	for in, want := range rates {
		if got, err := parseRate(in); err != nil || got != want {
			t.Errorf("parseRate(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "MiB", "-5MiB", "12XB", "abc", "1e9"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) succeeded", bad)
		}
	}
	if _, err := parseRate("fast"); err == nil {
		t.Error("parseRate(fast) succeeded")
	}
}

func TestComputeThroughput(t *testing.T) {
	const rate = 100 * mib
	// 30 s at a constant 100 MiB/s, with a 10 s stall in the middle replaced by 0.
	var samples []sample
	var done int64
	for ms := 0; ms <= 40_000; ms += 200 {
		at := time.Duration(ms) * time.Millisecond
		if at <= 12*time.Second || at > 22*time.Second {
			done += rate / 5
		}
		samples = append(samples, sample{at, done})
	}
	// done grew 100 MiB/s except for the stall from 12 s to 22 s.
	total := done
	tp := computeThroughput(samples, total, 40*time.Second)
	if want := float64(total) / 40; tp.WholeRun != want {
		t.Fatalf("whole run %v, want %v", tp.WholeRun, want)
	}
	if tp.SteadyState == nil || *tp.SteadyState < 0.6*float64(rate) || *tp.SteadyState > 0.8*float64(rate) {
		t.Fatalf("steady state %v", tp.SteadyState)
	}
	if tp.Slowest == nil || *tp.Slowest > 0.01*float64(rate) {
		t.Fatalf("slowest window %v, want about 0 inside the stall", tp.Slowest)
	}
	if tp.SlowestStart < 12 || tp.SlowestStart > 12.5 {
		t.Fatalf("slowest window starts at %v s, want 12", tp.SlowestStart)
	}

	// Constant rate: every figure equals the rate.
	flat := []sample{{0, 0}, {30 * time.Second, 30 * rate}}
	tp = computeThroughput(flat, 30*rate, 30*time.Second)
	for name, v := range map[string]float64{"whole": tp.WholeRun, "steady": *tp.SteadyState, "slowest": *tp.Slowest} {
		if v < float64(rate)*0.999 || v > float64(rate)*1.001 {
			t.Errorf("%s = %v, want %d", name, v, rate)
		}
	}

	// A run shorter than the skip has neither figure.
	short := computeThroughput([]sample{{0, 0}, {5 * time.Second, 5 * rate}}, 5*rate, 5*time.Second)
	if short.SteadyState != nil || short.Slowest != nil || short.WholeRun != float64(rate) {
		t.Fatalf("short run: %+v", short)
	}
	// Exactly 10 s: a slowest window exists, a steady state does not.
	ten := computeThroughput([]sample{{0, 0}, {10 * time.Second, 10 * rate}}, 10*rate, 10*time.Second)
	if ten.SteadyState != nil || ten.Slowest == nil {
		t.Fatalf("10 s run: %+v", ten)
	}
	if z := computeThroughput(nil, 0, 0); z.WholeRun != 0 {
		t.Fatalf("empty: %+v", z)
	}
}
