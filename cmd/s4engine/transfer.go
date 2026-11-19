package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fsx"
	"s4browser/internal/journal"
)

type transferFlags struct {
	commonFlags
	key           string
	destDir       string
	parallelFiles int
	parallelParts int
	totalParts    int
	partSize      string
	bandwidth     string
	strategy      string
	lead          string
	journalDir    string
	progress      time.Duration
	fakeSize      string
}

// settingsOut is the settings block of a result file.
type settingsOut struct {
	ParallelFiles        int    `json:"parallelFiles"`
	ParallelPartsPerFile int    `json:"parallelPartsPerFile"`
	TotalParallelParts   int    `json:"totalParallelParts"`
	PartSizeBytes        int64  `json:"partSizeBytes"`
	BandwidthBytesPerSec int64  `json:"bandwidthCapBytesPerSec"`
	WriteStrategy        string `json:"writeStrategy,omitempty"`
	WriteLeadBytes       int64  `json:"writeLeadBytes,omitempty"`
}

type fileResult struct {
	Key       string `json:"key"`
	LocalPath string `json:"localPath"`
	Bytes     int64  `json:"bytes"`
	DoneBytes int64  `json:"doneBytes"`
	State     string `json:"state"`
	Parts     int    `json:"parts"`
	Retries   int    `json:"retries"`
	Error     string `json:"error,omitempty"`
}

type targetOut struct {
	Kind    string `json:"kind"`
	Bucket  string `json:"bucket"`
	Region  string `json:"region"`
	Profile string `json:"profile,omitempty"`
	RunID   string `json:"runId"`
	Prefix  string `json:"prefix"`
}

// result is the JSON file written for every transfer. It holds no credentials:
// only names, sizes, timings and settings.
type result struct {
	Command    string       `json:"command"`
	Outcome    string       `json:"outcome"`
	Error      string       `json:"error,omitempty"`
	Commit     string       `json:"commit"`
	GoVersion  string       `json:"goVersion"`
	OS         string       `json:"os"`
	Arch       string       `json:"arch"`
	StartedAt  time.Time    `json:"startedAt"`
	Target     targetOut    `json:"target"`
	Settings   settingsOut  `json:"settings"`
	Files      []fileResult `json:"files"`
	Bytes      int64        `json:"bytes"`
	DurationS  float64      `json:"durationSeconds"`
	Throughput float64      `json:"throughputBytesPerSec"`
	Gbps       float64      `json:"throughputGbps"`
	// SteadyStateBytesPerSec excludes the first 10 s. Absent for shorter runs.
	SteadyState *float64 `json:"steadyStateBytesPerSec,omitempty"`
	// Slowest10sBytesPerSec is the lowest rate over any 10 s window. Absent for
	// runs shorter than 10 s.
	Slowest10s      *float64 `json:"slowest10sWindowBytesPerSec,omitempty"`
	Slowest10sStart *float64 `json:"slowest10sWindowStartSeconds,omitempty"`
	Retries         int      `json:"retries"`
	HeapBaseline    uint64   `json:"heapBaselineBytes"`
	PeakHeap        uint64   `json:"peakHeapBytes"`
	PeakWorkingSet  uint64   `json:"peakWorkingSetBytes"`
	// FakeVerified is set by upload --fake --seed after the fake recomputed the
	// CRC of every uploaded part from the pattern.
	FakeVerified *bool `json:"fakeVerified,omitempty"`
}

func cmdTransfer(ctx context.Context, e env, args []string, upload bool) (int, error) {
	command := "download"
	if upload {
		command = "upload"
	}
	var f transferFlags
	fs := newFlagSet(command, e)
	f.register(fs)
	fs.StringVar(&f.key, "key", "", "upload: destination key for the single file (default s4browser-test/<run-id>/<file name>)")
	fs.StringVar(&f.destDir, "dest-dir", "", "download: existing directory for the files (required)")
	fs.IntVar(&f.parallelFiles, "parallel-files", 0, "files transferred at once (default engine default)")
	fs.IntVar(&f.parallelParts, "parallel-parts", 0, "parts in flight per file (default engine default)")
	fs.IntVar(&f.totalParts, "total-parts", 0, "parts in flight overall (default engine default, raised to --parallel-parts if smaller)")
	fs.StringVar(&f.partSize, "part-size", "", "part size, e.g. 64MiB (default engine default)")
	fs.StringVar(&f.bandwidth, "bandwidth", "", "bandwidth cap, e.g. 5Gbit or 500MiB (default unlimited)")
	fs.StringVar(&f.strategy, "write-strategy", "", "download write strategy: sparse, sequential-lead, preallocate, valid-data (default sparse)")
	fs.StringVar(&f.lead, "write-lead", "", "download lead for sequential-lead and preallocate, e.g. 2GiB")
	fs.StringVar(&f.journalDir, "journal-dir", "", "journal directory (default: a temporary directory removed when empty at exit)")
	fs.StringVar(&f.fakeSize, "fake-size", "", "download with --fake: size of the synthetic objects to create at the keys, whose bytes follow --seed")
	fs.DurationVar(&f.progress, "progress", time.Second, "interval between progress lines")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage, err
	}
	guard, err := f.validate(command, false)
	if err != nil {
		return exitUsage, err
	}
	if len(positional) == 0 {
		if upload {
			return exitUsage, usagef("give at least one file to upload")
		}
		return exitUsage, usagef("give at least one object key to download")
	}
	settings, out, err := f.settings(upload)
	if err != nil {
		return exitUsage, err
	}
	var fakeSize int64
	if f.fakeSize != "" {
		if !f.fake || upload {
			return exitUsage, usagef("--fake-size is only for download with --fake")
		}
		if fakeSize, err = parseSize(f.fakeSize); err != nil {
			return exitUsage, usagef("--fake-size: %v", err)
		}
	}

	// Destinations are decided and checked before any credential is loaded or
	// connection is made. A key outside the run prefix ends the command here.
	var specs []engine.JobSpec
	var keys []string
	if upload {
		if f.key != "" && len(positional) != 1 {
			return exitUsage, usagef("--key needs exactly one file")
		}
		seen := map[string]bool{}
		for _, p := range positional {
			abs, err := filepath.Abs(p)
			if err != nil {
				return exitUsage, usagef("%v", err)
			}
			info, err := os.Stat(abs)
			if err != nil || !info.Mode().IsRegular() {
				return exitUsage, usagef("%s is not a readable file", p)
			}
			key := f.key
			if key == "" {
				key = guard.Prefix() + filepath.Base(abs)
			}
			if err := guard.CheckWrite(key); err != nil {
				return exitUsage, usagef("refusing to start: %v", err)
			}
			if seen[key] {
				return exitUsage, usagef("two files map to the key %q", key)
			}
			seen[key] = true
			keys = append(keys, key)
			specs = append(specs, engine.JobSpec{Direction: engine.Upload, LocalPath: abs})
		}
	} else {
		if f.destDir == "" {
			return exitUsage, usagef("--dest-dir is required")
		}
		dir, err := filepath.Abs(f.destDir)
		if err != nil {
			return exitUsage, usagef("%v", err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return exitUsage, usagef("--dest-dir %s is not an existing directory", f.destDir)
		}
		seen := map[string]bool{}
		for _, key := range positional {
			// Downloads only read, but the tool stays inside one prefix.
			if err := guard.CheckWrite(key); err != nil {
				return exitUsage, usagef("refusing to start: %v", err)
			}
			name := path.Base(key)
			if seen[name] {
				return exitUsage, usagef("two keys have the file name %q", name)
			}
			seen[name] = true
			keys = append(keys, key)
			specs = append(specs, engine.JobSpec{Direction: engine.Download, LocalPath: filepath.Join(dir, name)})
		}
	}

	tgt, err := newTarget(ctx, &f.commonFlags, guard, e)
	if err != nil {
		return exitFailed, err
	}
	defer tgt.close()
	for i := range specs {
		specs[i].Ref = tgt.ref(keys[i])
	}
	if !upload && tgt.fake != nil && fakeSize > 0 {
		// A fresh fake holds no objects, so create synthetic ones to download.
		for _, key := range keys {
			if err := tgt.fake.PutSynthetic(tgt.bucket, key, f.seed, fakeSize); err != nil {
				return exitFailed, err
			}
		}
	}
	return runTransfer(ctx, e, &f, tgt, command, specs, keys, settings, out)
}

// settings builds the engine settings from the flags.
func (f *transferFlags) settings(upload bool) (engine.Settings, settingsOut, error) {
	s := engine.DefaultSettings()
	if f.parallelFiles > 0 {
		s.MaxActiveFiles = f.parallelFiles
	}
	if f.parallelParts > 0 {
		s.MaxPartsPerFile = f.parallelParts
	}
	s.MaxActiveParts = max(engine.DefaultActiveParts, s.MaxPartsPerFile)
	if f.totalParts > 0 {
		s.MaxActiveParts = f.totalParts
	}
	if f.partSize != "" {
		n, err := parseSize(f.partSize)
		if err != nil {
			return s, settingsOut{}, usagef("--part-size: %v", err)
		}
		s.PartSize = n
	}
	var rate int64
	if f.bandwidth != "" {
		n, err := parseRate(f.bandwidth)
		if err != nil {
			return s, settingsOut{}, usagef("--bandwidth: %v", err)
		}
		rate = n
		if upload {
			s.UploadBytesPerSec = n
		} else {
			s.DownloadBytesPerSec = n
		}
	}
	if err := s.Validate(); err != nil {
		return s, settingsOut{}, usagef("%v", err)
	}
	out := settingsOut{
		ParallelFiles: s.MaxActiveFiles, ParallelPartsPerFile: s.MaxPartsPerFile, TotalParallelParts: s.MaxActiveParts,
		PartSizeBytes: s.PartSize, BandwidthBytesPerSec: rate,
	}
	return s, out, nil
}

// sink returns the download sink and records its strategy in out.
func (f *transferFlags) sink(out *settingsOut) (engine.Sink, error) {
	strategy := fsx.DefaultStrategy()
	if f.strategy != "" || f.lead != "" {
		name := f.strategy
		if name == "" {
			name = fsx.DefaultStrategy().Name()
		}
		var lead int64
		if f.lead != "" {
			n, err := parseSize(f.lead)
			if err != nil {
				return nil, usagef("--write-lead: %v", err)
			}
			lead = n
		}
		var err error
		if strategy, err = fsx.NewStrategy(name, lead); err != nil {
			return nil, usagef("--write-strategy: %v", err)
		}
	}
	out.WriteStrategy, out.WriteLeadBytes = strategy.Name(), strategy.MaxLead()
	return engine.NewFileSink(strategy), nil
}

func runTransfer(ctx context.Context, e env, f *transferFlags, tgt *target, command string, specs []engine.JobSpec, keys []string, settings engine.Settings, sout settingsOut) (int, error) {
	upload := command == "upload"
	var sink engine.Sink = engine.NewFileSink(fsx.DefaultStrategy())
	if !upload {
		var err error
		if sink, err = f.sink(&sout); err != nil {
			return exitUsage, err
		}
	}

	journalDir, ownJournal := f.journalDir, f.journalDir == ""
	if ownJournal {
		dir, err := os.MkdirTemp("", "s4engine-journal-")
		if err != nil {
			return exitFailed, err
		}
		journalDir = dir
	}
	store, err := journal.OpenFile(journalDir)
	if err != nil {
		return exitFailed, fmt.Errorf("open journal: %w", err)
	}

	eng, err := engine.New(engine.Config{
		Store: tgt.store, Source: engine.FileSource{}, Sink: sink, Journal: store,
		Prober: tgt.prober, Clock: engine.RealClock{}, Guard: tgt.guard,
		Jitter: rand.Float64, Settings: settings, Retry: engine.DefaultRetryPolicy(), Timeouts: engine.DefaultTimeouts(),
	})
	if err != nil {
		_ = store.Close()
		return exitFailed, err
	}

	mem := startMemorySampler()
	startedAt := time.Now()
	start := startedAt
	ids := make([]engine.JobID, 0, len(specs))
	for _, spec := range specs {
		id, err := eng.Submit(context.Background(), spec)
		if err != nil {
			shutdown(eng, store, journalDir, ownJournal, e)
			return exitFailed, fmt.Errorf("submit: %w", err)
		}
		ids = append(ids, id)
	}

	samples := []sample{{0, 0}}
	var snaps []engine.JobSnapshot
	interrupted := false
	tick := time.NewTicker(200 * time.Millisecond)
	nextLine := start.Add(f.progress)
	var prev sample
	for {
		select {
		case <-ctx.Done():
			interrupted = true
		case <-tick.C:
		}
		if interrupted {
			fmt.Fprintln(e.stderr, "interrupted: cancelling and aborting multipart uploads...")
			for _, id := range ids {
				if err := eng.Cancel(context.Background(), id); err != nil && !errors.Is(err, engine.ErrIllegalState) {
					fmt.Fprintf(e.stderr, "cancel %s: %v\n", id, err)
				}
			}
			snaps = waitTerminal(eng, 30*time.Second)
			break
		}
		snaps, err = eng.Snapshot(context.Background())
		if err != nil {
			tick.Stop()
			shutdown(eng, store, journalDir, ownJournal, e)
			return exitFailed, err
		}
		now := time.Since(start)
		done, total, allDone := tally(snaps)
		samples = append(samples, sample{now, done})
		if time.Now().After(nextLine) || allDone {
			printProgress(e, now, done, total, prev, snaps)
			prev, nextLine = sample{now, done}, time.Now().Add(f.progress)
		}
		if allDone {
			break
		}
	}
	tick.Stop()
	duration := time.Since(start)
	done, _, _ := tally(snaps)
	// The last poll can be up to one tick before the end of the run.
	samples = append(samples, sample{duration, done})

	res := result{
		Command: command, Commit: buildCommit(), GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		StartedAt: startedAt.UTC(), Settings: sout,
		Target: targetOut{Kind: tgt.kind, Bucket: tgt.bucket, Region: tgt.region, Profile: tgt.profile, RunID: f.runID, Prefix: tgt.guard.Prefix()},
	}
	failed := 0
	completedBytes := int64(0)
	for i, s := range snaps {
		fr := fileResult{Key: keys[i], LocalPath: specs[i].LocalPath, Bytes: s.TotalBytes, DoneBytes: s.DoneBytes, State: s.State.String(), Parts: s.PartsTotal, Retries: s.Retries}
		if s.State == engine.JobCompleted {
			completedBytes += s.TotalBytes
		} else {
			failed++
			if s.Err != nil {
				fr.Error = fmt.Sprintf("%s: %s", s.Err.Code, s.Err.Message)
			}
		}
		res.Retries += s.Retries
		res.Files = append(res.Files, fr)
	}
	res.Bytes = completedBytes
	res.DurationS = duration.Seconds()
	tp := computeThroughput(samples, done, duration)
	res.Throughput, res.Gbps = tp.WholeRun, tp.WholeRun*8/1e9
	res.SteadyState, res.Slowest10s = tp.SteadyState, tp.Slowest
	if tp.Slowest != nil {
		start := tp.SlowestStart
		res.Slowest10sStart = &start
	}
	switch {
	case interrupted:
		res.Outcome = "cancelled"
	case failed > 0:
		res.Outcome = "failed"
	default:
		res.Outcome = "completed"
	}

	// A fake upload given the content seed is checked by the fake, which
	// recomputes every part's CRC32 from the pattern.
	if upload && tgt.fake != nil && f.seed != 0 && res.Outcome == "completed" {
		ok := true
		for _, key := range keys {
			if err := tgt.fake.VerifyObject(tgt.bucket, key, f.seed, 1); err != nil {
				ok = false
				fmt.Fprintf(e.stderr, "fake verify %s: %v\n", key, err)
			}
		}
		res.FakeVerified = &ok
		if !ok {
			res.Outcome, res.Error = "failed", "stored object does not match the seed"
			failed++
		}
	}

	shutdown(eng, store, journalDir, ownJournal, e)
	// The OS peak and the heap peak are read after the engine has stopped, so
	// they cover the whole run including shutdown.
	res.HeapBaseline, res.PeakHeap = mem.finish()
	res.PeakWorkingSet = peakWorkingSet()

	if err := writeJSON(f.result, res); err != nil {
		fmt.Fprintf(e.stderr, "write result: %v\n", err)
		return exitFailed, nil
	}
	printSummary(e, res, f.result)
	for _, fr := range res.Files {
		if fr.Error != "" {
			fmt.Fprintf(e.stderr, "%s: %s\n", fr.Key, fr.Error)
		}
	}
	switch {
	case interrupted:
		return exitInterrupted, nil
	case failed > 0:
		return exitFailed, nil
	}
	return exitOK, nil
}

// shutdown closes the engine and the journal. A journal that still holds
// entries (an abort that failed) is kept and reported, because it names
// multipart uploads that may still exist on S3.
func shutdown(eng engine.Engine, store *journal.FileStore, dir string, own bool, e env) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := eng.Close(ctx); err != nil {
		fmt.Fprintf(e.stderr, "engine close: %v\n", err)
	}
	left := store.List()
	_ = store.Close()
	switch {
	case len(left) > 0:
		fmt.Fprintf(e.stderr, "WARNING: %d unfinished journal entr(ies) kept in %s. Run cleanup for this run ID to abort leftover uploads.\n", len(left), dir)
	case own:
		_ = os.RemoveAll(dir)
	}
}

// waitTerminal polls until every job is terminal or the timeout passes.
func waitTerminal(eng engine.Engine, timeout time.Duration) []engine.JobSnapshot {
	deadline := time.Now().Add(timeout)
	for {
		snaps, _ := eng.Snapshot(context.Background())
		if _, _, all := tally(snaps); all || time.Now().After(deadline) {
			return snaps
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tally sums the byte counts and reports whether every job is terminal.
func tally(snaps []engine.JobSnapshot) (done, total int64, allTerminal bool) {
	allTerminal = len(snaps) > 0
	for _, s := range snaps {
		done += s.DoneBytes
		total += s.TotalBytes
		allTerminal = allTerminal && s.State.IsTerminal()
	}
	return
}

func printProgress(e env, at time.Duration, done, total int64, prev sample, snaps []engine.JobSnapshot) {
	rate := 0.0
	if dt := at - prev.at; dt > 0 {
		rate = float64(done-prev.done) / dt.Seconds()
	}
	pct := 0.0
	if total > 0 {
		pct = 100 * float64(done) / float64(total)
	}
	retries, active := 0, 0
	for _, s := range snaps {
		retries += s.Retries
		if s.State == engine.JobRunning {
			active++
		}
	}
	fmt.Fprintf(e.stderr, "[%6.1fs] %s / %s (%.1f%%)  %s/s  %.2f Gbps  files running %d  retries %d\n",
		at.Seconds(), engine.FormatBytes(done), engine.FormatBytes(total), pct,
		engine.FormatBytes(int64(rate)), rate*8/1e9, active, retries)
}

func printSummary(e env, r result, resultPath string) {
	fmt.Fprintf(e.stdout, "%s %s: %s in %.1fs, %s/s (%.2f Gbps)\n", r.Command, r.Outcome,
		engine.FormatBytes(r.Bytes), r.DurationS, engine.FormatBytes(int64(r.Throughput)), r.Gbps)
	if r.SteadyState != nil {
		fmt.Fprintf(e.stdout, "  steady state (after 10 s): %s/s (%.2f Gbps)\n", engine.FormatBytes(int64(*r.SteadyState)), *r.SteadyState*8/1e9)
	}
	if r.Slowest10s != nil {
		fmt.Fprintf(e.stdout, "  slowest 10 s window: %s/s at %.0f s\n", engine.FormatBytes(int64(*r.Slowest10s)), *r.Slowest10sStart)
	}
	fmt.Fprintf(e.stdout, "  retries %d, peak heap %s, peak working set %s\n", r.Retries, engine.FormatBytes(int64(r.PeakHeap)), engine.FormatBytes(int64(r.PeakWorkingSet)))
	fmt.Fprintf(e.stdout, "  run ID %s, result file %s\n", r.Target.RunID, resultPath)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
