package engine_test

// Download write-strategy benchmark (stage 0.5). It runs the real engine, with
// the real scheduler and file sink, against the in-process fake S3, which feeds
// synthetic data far faster than 5 Gbps. The disk is therefore the bottleneck,
// and the writes arrive in the order the scheduler really produces: several parts
// of one file in flight at once, each at its own offset.
//
// It is skipped unless S4_BENCH_DIR is set, and it only ever writes inside
// F:\s4browser-bench. The environment variables that configure it are listed above TestDownloadWriteBenchmark.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/fsx"
	"s4browser/internal/journal"
)

// benchRoot is the only place the benchmark writes. Anything else is refused.
const benchRoot = "F:\\s4browser-bench"

func benchEnv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func benchInt(t *testing.T, name string, fallback int64) int64 {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	mult := int64(1)
	for suffix, m := range map[string]int64{"GiB": 1 << 30, "MiB": 1 << 20, "KiB": 1 << 10, "GB": 1_000_000_000} {
		if strings.HasSuffix(v, suffix) {
			mult, v = m, strings.TrimSuffix(v, suffix)
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("%s=%q: %v", name, os.Getenv(name), err)
	}
	return int64(n * float64(mult))
}

// benchTarget times every call into the strategy's file.
type benchTarget struct {
	inner fsx.WriteTarget
	m     *benchMeasure
}

func (b *benchTarget) WriteAt(p []byte, off int64) (int, error) {
	start := time.Now()
	n, err := b.inner.WriteAt(p, off)
	end := time.Now()
	b.m.recordWrite(start, end, n)
	return n, err
}

func (b *benchTarget) Sync() error {
	start := time.Now()
	err := b.inner.Sync()
	b.m.syncTime.Store(int64(time.Since(start)))
	return err
}

func (b *benchTarget) Close() error {
	start := time.Now()
	err := b.inner.Close()
	b.m.closeTime.Store(int64(time.Since(start)))
	return err
}

func (b *benchTarget) Finish() error {
	f, ok := b.inner.(fsx.Finisher)
	if !ok {
		return nil
	}
	start := time.Now()
	err := f.Finish()
	b.m.finishTime.Store(int64(time.Since(start)))
	return err
}

type benchStrategy struct {
	fsx.WriteStrategy
	m *benchMeasure
}

func (s benchStrategy) Create(path string, size int64) (fsx.WriteTarget, error) {
	start := time.Now()
	target, err := s.WriteStrategy.Create(path, size)
	s.m.createTime.Store(int64(time.Since(start)))
	if err != nil {
		return nil, err
	}
	return &benchTarget{inner: target, m: s.m}, nil
}

// benchMeasure collects what the report needs. Writes are recorded under a mutex,
// which is cheap at about 1,000 writes per second.
type benchMeasure struct {
	origin time.Time

	mu        sync.Mutex
	latencies []int64 // ns
	seconds   []int64 // bytes completed in each second since the first write
	lastDone  int64   // ns since origin
	worstGap  int64
	firstDone int64
	bytes     int64

	createTime, syncTime, closeTime, finishTime, commitTime atomic.Int64
}

func (m *benchMeasure) recordWrite(start, end time.Time, n int) {
	lat := end.Sub(start).Nanoseconds()
	done := end.Sub(m.origin).Nanoseconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latencies = append(m.latencies, lat)
	if m.firstDone == 0 {
		m.firstDone = done
	}
	if m.lastDone != 0 && done-m.lastDone > m.worstGap {
		m.worstGap = done - m.lastDone
	}
	if done > m.lastDone {
		m.lastDone = done
	}
	sec := int((done - m.firstDone) / int64(time.Second))
	for len(m.seconds) <= sec {
		m.seconds = append(m.seconds, 0)
	}
	m.seconds[sec] += int64(n)
	m.bytes += int64(n)
}

// benchSink adds the commit timing to the file sink.
type benchSink struct {
	inner engine.Sink
	m     *benchMeasure
}

func (s benchSink) Prepare(ctx context.Context, spec engine.SinkSpec) (engine.SinkWriter, error) {
	w, err := s.inner.Prepare(ctx, spec)
	if err != nil {
		return nil, err
	}
	return benchWriter{w, s.m}, nil
}

type benchWriter struct {
	engine.SinkWriter
	m *benchMeasure
}

func (w benchWriter) Commit(ctx context.Context) error {
	start := time.Now()
	err := w.SinkWriter.Commit(ctx)
	w.m.commitTime.Store(int64(time.Since(start)))
	return err
}

func benchHeap() int64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return int64(sample[0].Value.Uint64())
}

type benchResult struct {
	Strategy     string  `json:"strategy"`
	LeadMiB      int64   `json:"leadMiB"`
	SizeGiB      float64 `json:"sizeGiB"`
	PartMiB      int64   `json:"partMiB"`
	Files        int     `json:"files"`
	PartsPerFile int     `json:"partsPerFile"`
	RateMiBs     int64   `json:"rateLimitMiBs"`

	TransferSec     float64 `json:"transferSec"`
	MiBps           float64 `json:"sustainedMiBps"`
	Gbps            float64 `json:"sustainedGbps"`
	MinSecMiBps     float64 `json:"slowestSecondMiBps"`
	P5SecMiBps      float64 `json:"p5SecondMiBps"`
	MedSecMiBps     float64 `json:"medianSecondMiBps"`
	FirstThirdMiBps float64 `json:"firstThirdMiBps"`
	LastThirdMiBps  float64 `json:"lastThirdMiBps"`
	Seconds0to10pct float64 `json:"slowestTenSecondMiBps"`
	WorstGapMs      float64 `json:"worstStallMs"`
	MaxWriteMs      float64 `json:"maxWriteMs"`
	P50WriteMs      float64 `json:"p50WriteMs"`
	P99WriteMs      float64 `json:"p99WriteMs"`
	P999WriteMs     float64 `json:"p999WriteMs"`
	Writes          int     `json:"writes"`
	PeakHeapMiB     float64 `json:"peakHeapMiB"`
	PeakWorkingMB   float64 `json:"peakWorkingSetMiB"`
	CreateMs        float64 `json:"createMs"`
	FinalizeMs      float64 `json:"finalizeMs"`
	FinishMs        float64 `json:"finishMs"`
	SyncMs          float64 `json:"syncMs"`
	CloseMs         float64 `json:"closeMs"`
	Extents         int     `json:"extents"`
	Retries         int     `json:"retries"`
	Parks           int64   `json:"parks"`
	ParkedMaxMs     float64 `json:"parkedMaxMs"`
	Preemptions     int64   `json:"preemptions"`
	Verified        string  `json:"verified"`
}

func ms(ns int64) float64 { return float64(ns) / 1e6 }

// TestDownloadWriteBenchmark is driven by environment variables:
//
//	S4_BENCH_DIR       directory under F:\s4browser-bench (required to run)
//	S4_BENCH_STRATEGY  sequential-lead, preallocate, sparse or valid-data
//	S4_BENCH_SIZE      for example 20GiB
//	S4_BENCH_LEAD      MaxLead for the strategies that use one, for example 2GiB
//	S4_BENCH_PART      part size, default 64MiB
//	S4_BENCH_FILES, S4_BENCH_PARTS   active files and parts per file, default 1 and 16
//	S4_BENCH_RATE      feed limit in bytes per second, default unlimited
//	S4_BENCH_MINFREE   refuse to start with less free space than this, for example 350GiB
//	S4_BENCH_RESULTS   file that receives the result as one JSON line
func TestDownloadWriteBenchmark(t *testing.T) {
	dirName := os.Getenv("S4_BENCH_DIR")
	if dirName == "" {
		t.Skip("set S4_BENCH_DIR to run the download write benchmark")
	}
	dir := filepath.Clean(dirName)
	if !strings.EqualFold(dir, benchRoot) && !strings.HasPrefix(strings.ToLower(dir), strings.ToLower(benchRoot)+"\\") {
		t.Fatalf("refusing to write outside %s: %s", benchRoot, dir)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	size := benchInt(t, "S4_BENCH_SIZE", 8<<30)
	if need := benchInt(t, "S4_BENCH_MINFREE", 0); need > 0 {
		free, err := fsx.FreeSpace(dir)
		if err != nil || free < need {
			t.Fatalf("free space %d is below the required %d (%v)", free, need, err)
		}
	}
	name := benchEnv("S4_BENCH_STRATEGY", fsx.StrategySparse)
	lead := benchInt(t, "S4_BENCH_LEAD", 0)
	strategy, err := fsx.NewStrategy(name, lead)
	if err != nil {
		t.Skipf("strategy not available: %v", err)
	}

	// Only the files this run creates are removed, whatever the outcome.
	dest := filepath.Join(dir, fmt.Sprintf("bench-%s-%d.bin", name, os.Getpid()))
	journalDir := filepath.Join(dir, fmt.Sprintf("journal-%d", os.Getpid()))
	t.Cleanup(func() {
		_ = os.Remove(dest)
		if matches, _ := filepath.Glob(filepath.Join(dir, "~s4-*.s4tmp")); len(matches) > 0 {
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
		_ = os.RemoveAll(journalDir)
	})
	if err := os.MkdirAll(journalDir, 0o777); err != nil {
		t.Fatal(err)
	}

	measure := &benchMeasure{origin: time.Now()}
	server, err := fakes3.New(fakes3.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const key = "bench/object.bin"
	if err := server.PutSynthetic(fakes3.DefaultBucket, key, testSeed, size); err != nil {
		t.Fatal(err)
	}
	store, err := journal.OpenFile(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	settings := engine.DefaultSettings()
	settings.PartSize = benchInt(t, "S4_BENCH_PART", 64<<20)
	settings.MaxActiveFiles = int(benchInt(t, "S4_BENCH_FILES", 1))
	settings.MaxPartsPerFile = int(benchInt(t, "S4_BENCH_PARTS", 16))
	settings.MaxActiveParts = max(settings.MaxActiveFiles*settings.MaxPartsPerFile, 1)
	settings.DownloadBytesPerSec = benchInt(t, "S4_BENCH_RATE", 0)

	sink := benchSink{inner: engine.NewFileSink(benchStrategy{strategy, measure}), m: measure}
	eng, err := engine.New(engine.Config{
		Store: server.Store(), Source: fakes3.NewSource(), Sink: sink, Journal: store, Prober: new(scriptedProber),
		Clock: engine.RealClock{}, Jitter: func() float64 { return 0.5 }, Settings: settings,
		Retry: engine.RetryPolicy{BaseDelay: 500 * time.Millisecond, ThrottleBaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second,
			MaxPartAttempts: 10, MaxControlAttempts: 8, MaxAbortAttempts: 5, OutageTolerance: 30 * time.Minute},
		Timeouts: engine.DefaultTimeouts(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = eng.Close(ctx)
	}()

	// Peak memory is sampled every 100 ms.
	runtime.GC()
	baseHeap := benchHeap()
	var peakHeap, peakWS atomic.Int64
	stopSampling := make(chan struct{})
	var sampling sync.WaitGroup
	sampling.Go(func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if v := benchHeap(); v > peakHeap.Load() {
				peakHeap.Store(v)
			}
			if v := processPeakWorkingSet(); v > peakWS.Load() {
				peakWS.Store(v)
			}
			select {
			case <-ticker.C:
			case <-stopSampling:
				return
			}
		}
	})

	ref := engine.ObjectRef{Profile: "bench", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: key}
	measure.origin = time.Now()
	id, err := eng.Submit(context.Background(), engine.JobSpec{Direction: engine.Download, Ref: ref, LocalPath: dest})
	if err != nil {
		t.Fatal(err)
	}
	_ = id
	var final engine.JobSnapshot
	for {
		all, err := eng.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		final = all[0]
		if final.State.IsTerminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stopSampling)
	sampling.Wait()
	if final.State != engine.JobCompleted {
		t.Fatalf("job ended %s: %+v", final.State, final.Err)
	}

	measure.mu.Lock()
	defer measure.mu.Unlock()
	transfer := time.Duration(measure.lastDone - measure.firstDone)
	lat := slices.Clone(measure.latencies)
	slices.Sort(lat)
	pct := func(p float64) int64 { return lat[min(int(float64(len(lat))*p), len(lat)-1)] }
	// The first and last second are partial, so the per-second figures skip them.
	var secs []float64
	if len(measure.seconds) > 2 {
		for _, b := range measure.seconds[1 : len(measure.seconds)-1] {
			secs = append(secs, float64(b)/float64(1<<20))
		}
	}
	// Average rate of the first and last third of the run shows a drive that slows down.
	var first, last, slowTen float64
	if n := len(secs); n >= 3 {
		for i, v := range secs {
			switch {
			case i < n/3:
				first += v
			case i >= n-n/3:
				last += v
			}
		}
		first, last = first/float64(n/3), last/float64(n/3)
		// Slowest 10-second window.
		slowTen = -1
		for i := 0; i+10 <= n; i++ {
			sum := 0.0
			for _, v := range secs[i : i+10] {
				sum += v
			}
			if slowTen < 0 || sum/10 < slowTen {
				slowTen = sum / 10
			}
		}
	}
	slices.Sort(secs)
	at := func(p float64) float64 {
		if len(secs) == 0 {
			return 0
		}
		return secs[min(int(float64(len(secs))*p), len(secs)-1)]
	}
	stats := eng.(engine.StatsProvider).DownloadStats()
	res := benchResult{
		Strategy: name, LeadMiB: strategy.MaxLead() >> 20, SizeGiB: float64(size) / (1 << 30), PartMiB: settings.PartSize >> 20,
		Files: settings.MaxActiveFiles, PartsPerFile: settings.MaxPartsPerFile, RateMiBs: settings.DownloadBytesPerSec >> 20,
		TransferSec: transfer.Seconds(), MiBps: float64(measure.bytes) / float64(1<<20) / transfer.Seconds(),
		MinSecMiBps: at(0), P5SecMiBps: at(0.05), MedSecMiBps: at(0.5), FirstThirdMiBps: first, LastThirdMiBps: last, Seconds0to10pct: slowTen,
		WorstGapMs: ms(measure.worstGap), MaxWriteMs: ms(lat[len(lat)-1]), P50WriteMs: ms(pct(0.5)),
		P99WriteMs: ms(pct(0.99)), P999WriteMs: ms(pct(0.999)), Writes: len(lat),
		PeakHeapMiB: float64(peakHeap.Load()-baseHeap) / (1 << 20), PeakWorkingMB: float64(peakWS.Load()) / (1 << 20),
		CreateMs: ms(measure.createTime.Load()), FinalizeMs: ms(measure.commitTime.Load()), FinishMs: ms(measure.finishTime.Load()),
		SyncMs: ms(measure.syncTime.Load()), CloseMs: ms(measure.closeTime.Load()),
		Retries: final.Retries, Parks: stats.Parks, ParkedMaxMs: float64(stats.ParkedMax) / 1e6, Preemptions: stats.Preemptions,
	}
	res.Gbps = res.MiBps * float64(1<<20) * 8 / 1e9
	if measure.bytes != size {
		t.Fatalf("the sink received %d bytes, want %d", measure.bytes, size)
	}
	if info, err := os.Stat(dest); err != nil || info.Size() != size {
		t.Fatalf("final file: %v %v", info, err)
	}
	res.Extents = extentCount(dest)
	res.Verified = verifyBenchFile(t, dest, size)

	line, _ := json.Marshal(res)
	t.Logf("RESULT %s", line)
	if path := os.Getenv("S4_BENCH_RESULTS"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
}

// verifyBenchFile compares the finished file with the synthetic pattern. Files up
// to 32 GiB are read in full. Bigger ones are checked at 2,000 random places and
// at both ends, which reads about 2 GiB.
func verifyBenchFile(t *testing.T, path string, size int64) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf, scratch := make([]byte, 1<<20), make([]byte, 1<<20)
	check := func(off int64) {
		n := int(min(int64(len(buf)), size-off))
		if _, err := f.ReadAt(buf[:n], off); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		if at, ok := (fakes3.Pattern{Seed: testSeed}).Equal(buf[:n], off, scratch); !ok {
			t.Fatalf("file differs from the source at byte %d", at)
		}
	}
	if size <= 32<<30 {
		for off := int64(0); off < size; off += int64(len(buf)) {
			check(off)
		}
		return "full"
	}
	check(0)
	check((size - 1) &^ (1<<20 - 1))
	r := rand.New(rand.NewPCG(7, 7))
	for range 2000 {
		check(r.Int64N(size/(1<<20)) << 20)
	}
	return "sampled 2000 MiB"
}
