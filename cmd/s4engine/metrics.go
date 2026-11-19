package main

import (
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"time"
)

// sample is the transfer's cumulative byte count at an instant, measured from
// the start of the run.
type sample struct {
	at   time.Duration
	done int64
}

// Window sizes of the steady-state and slowest-window figures.
const (
	steadySkip  = 10 * time.Second
	slowWindow  = 10 * time.Second
	heapMetric  = "/memory/classes/heap/objects:bytes"
	memInterval = 100 * time.Millisecond
)

// throughput holds the rates derived from a series of samples.
type throughput struct {
	WholeRun float64 // bytes per second over the whole run
	// SteadyState excludes the first steadySkip. nil when the run was shorter.
	SteadyState *float64
	// Slowest is the lowest rate over any slowWindow, nil when the run was shorter.
	Slowest      *float64
	SlowestStart float64 // seconds from the start of the run
}

// computeThroughput derives the rates. samples must be sorted by time, begin at
// (0, 0) and end at (duration, total). Bytes between two samples are taken to
// arrive at a constant rate.
func computeThroughput(samples []sample, total int64, duration time.Duration) throughput {
	var t throughput
	if duration <= 0 {
		return t
	}
	t.WholeRun = float64(total) / duration.Seconds()
	if duration > steadySkip {
		v := float64(total-doneAt(samples, steadySkip)) / (duration - steadySkip).Seconds()
		t.SteadyState = &v
	}
	if duration >= slowWindow {
		best, bestStart := -1.0, time.Duration(0)
		try := func(start time.Duration) {
			if start < 0 || start+slowWindow > duration {
				return
			}
			rate := float64(doneAt(samples, start+slowWindow)-doneAt(samples, start)) / slowWindow.Seconds()
			if best < 0 || rate < best {
				best, bestStart = rate, start
			}
		}
		try(0)
		for _, s := range samples {
			try(s.at)
			try(s.at - slowWindow)
		}
		if best >= 0 {
			t.Slowest = &best
			t.SlowestStart = bestStart.Seconds()
		}
	}
	return t
}

// doneAt returns the cumulative bytes at time at, interpolating linearly.
func doneAt(samples []sample, at time.Duration) int64 {
	if len(samples) == 0 {
		return 0
	}
	if at <= samples[0].at {
		return samples[0].done
	}
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		if at <= b.at {
			if b.at == a.at {
				return b.done
			}
			frac := float64(at-a.at) / float64(b.at-a.at)
			return a.done + int64(frac*float64(b.done-a.done))
		}
	}
	return samples[len(samples)-1].done
}

// memorySampler records the peak live Go heap while a transfer runs.
type memorySampler struct {
	baseline uint64
	mu       sync.Mutex
	peak     uint64
	stop     chan struct{}
	done     chan struct{}
}

func readHeap() uint64 {
	s := []metrics.Sample{{Name: heapMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

// startMemorySampler takes a baseline after a forced collection and then
// samples every memInterval.
func startMemorySampler() *memorySampler {
	runtime.GC()
	m := &memorySampler{baseline: readHeap(), stop: make(chan struct{}), done: make(chan struct{})}
	m.peak = m.baseline
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(memInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.observe()
			case <-m.stop:
				m.observe()
				return
			}
		}
	}()
	return m
}

func (m *memorySampler) observe() {
	h := readHeap()
	m.mu.Lock()
	m.peak = max(m.peak, h)
	m.mu.Unlock()
}

// finish stops sampling and returns the baseline and peak heap in bytes.
func (m *memorySampler) finish() (baseline, peak uint64) {
	close(m.stop)
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseline, m.peak
}

// buildCommit returns the VCS revision the binary was built from, with
// "+dirty" when the tree had uncommitted changes, or "unknown" for a build
// that carries none (go test, go run).
func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
