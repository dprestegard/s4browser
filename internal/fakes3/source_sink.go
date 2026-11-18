package fakes3

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"sync"
	"time"

	"s4browser/internal/engine"
)

// SyntheticFile defines an upload source by its content pattern.
type SyntheticFile struct {
	Size    int64
	Seed    uint64
	ModTime time.Time
	// FailAt, when > 0, makes reads that reach this offset fail with FailErr,
	// like a disk error part-way through a file.
	FailAt  int64
	FailErr error
}

// Source is an engine.Source whose files are synthesized from their offset, so a
// simulated 2 TB upload reads no disk.
type Source struct {
	mu    sync.RWMutex
	files map[string]SyntheticFile
}

var _ engine.Source = (*Source)(nil)

// NewSource returns an empty source.
func NewSource() *Source { return &Source{files: make(map[string]SyntheticFile)} }

// Set defines or replaces the file at path. Open handles see the change on
// their next Stat, which is how tests simulate a file edited during upload.
func (s *Source) Set(path string, f SyntheticFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.ModTime.IsZero() {
		f.ModTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	s.files[path] = f
}

func (s *Source) get(path string) (SyntheticFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.files[path]
	return f, ok
}

// Open implements engine.Source.
func (s *Source) Open(ctx context.Context, path string) (engine.SourceHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := s.get(path); !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return &sourceHandle{src: s, path: path}, nil
}

type sourceHandle struct {
	src  *Source
	path string
}

func (h *sourceHandle) ReadAt(p []byte, off int64) (int, error) {
	f, ok := h.src.get(h.path)
	if !ok {
		return 0, &fs.PathError{Op: "read", Path: h.path, Err: fs.ErrNotExist}
	}
	if off < 0 {
		return 0, fmt.Errorf("fakes3: negative offset %d", off)
	}
	if off >= f.Size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), f.Size-off))
	var failErr error
	if f.FailAt > 0 && off+int64(n) > f.FailAt {
		if off >= f.FailAt {
			return 0, f.FailErr
		}
		n, failErr = int(f.FailAt-off), f.FailErr
	}
	Pattern{Seed: f.Seed}.Fill(p[:n], off)
	switch {
	case failErr != nil:
		return n, failErr
	case n < len(p):
		return n, io.EOF
	}
	return n, nil
}

func (h *sourceHandle) Stat() (engine.SourceStat, error) {
	f, ok := h.src.get(h.path)
	if !ok {
		return engine.SourceStat{}, &fs.PathError{Op: "stat", Path: h.path, Err: fs.ErrNotExist}
	}
	return engine.SourceStat{Size: f.Size, ModTime: f.ModTime}, nil
}

func (h *sourceHandle) Close() error { return nil }

// Sink is an engine.Sink that verifies downloaded bytes against a pattern and
// stores nothing. Each writer tracks which byte ranges arrived, in a merged
// interval list, so memory does not grow with the object size.
type Sink struct {
	// Seed returns the pattern a download to path must contain.
	Seed func(path string) uint64
	// MaxLead is reported by every writer.
	MaxLead int64
	// FreeSpace, when > 0, makes Prepare fail with ErrInsufficientSpace for
	// larger objects.
	FreeSpace int64

	mu        sync.Mutex
	existing  map[string]bool
	committed map[string]bool
	aborted   map[string]bool
	writers   map[string]*sinkWriter
}

var _ engine.Sink = (*Sink)(nil)

// NewSink returns a sink verifying every download against Pattern{seed(path)}.
func NewSink(seed func(path string) uint64) *Sink {
	return &Sink{Seed: seed, existing: map[string]bool{}, committed: map[string]bool{}, aborted: map[string]bool{}, writers: map[string]*sinkWriter{}}
}

// MarkExisting makes path look like it already exists, so Prepare and Commit
// report engine.ErrDestinationExists.
func (s *Sink) MarkExisting(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existing[path] = true
}

// Committed reports whether a download to path was committed.
func (s *Sink) Committed(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed[path]
}

// Aborted reports whether a download to path was aborted.
func (s *Sink) Aborted(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted[path]
}

// BytesWritten returns the total bytes passed to WriteAt for path, counting
// rewritten bytes twice, and the number of distinct bytes covered.
func (s *Sink) BytesWritten(path string) (total, distinct int64) {
	s.mu.Lock()
	w := s.writers[path]
	s.mu.Unlock()
	if w == nil {
		return 0, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, iv := range w.covered {
		distinct += iv[1] - iv[0]
	}
	return w.total, distinct
}

// Prepare implements engine.Sink.
func (s *Sink) Prepare(ctx context.Context, spec engine.SinkSpec) (engine.SinkWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.existing[spec.DestPath] || s.committed[spec.DestPath] {
		return nil, engine.ErrDestinationExists
	}
	if s.FreeSpace > 0 && spec.Size > s.FreeSpace {
		return nil, engine.ErrInsufficientSpace
	}
	w := &sinkWriter{sink: s, spec: spec, seed: s.Seed(spec.DestPath)}
	s.writers[spec.DestPath] = w
	delete(s.aborted, spec.DestPath)
	return w, nil
}

type sinkWriter struct {
	sink *Sink
	spec engine.SinkSpec
	seed uint64

	mu      sync.Mutex
	covered [][2]int64 // sorted, merged [start, end) ranges
	total   int64
	ended   bool
}

var scratchPool = sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}

func (w *sinkWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > w.spec.Size {
		return 0, fmt.Errorf("fakes3: write [%d,%d) outside object of %d bytes", off, off+int64(len(p)), w.spec.Size)
	}
	sp := scratchPool.Get().(*[]byte)
	diff, ok := Pattern{Seed: w.seed}.Equal(p, off, *sp)
	scratchPool.Put(sp)
	if !ok {
		return 0, fmt.Errorf("fakes3: downloaded bytes differ from the synthetic source at offset %d", diff)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return 0, fmt.Errorf("fakes3: write after commit or abort")
	}
	w.total += int64(len(p))
	w.cover(off, off+int64(len(p)))
	return len(p), nil
}

// cover merges [start, end) into the interval list.
func (w *sinkWriter) cover(start, end int64) {
	if start >= end {
		return
	}
	i := sort.Search(len(w.covered), func(i int) bool { return w.covered[i][1] >= start })
	j := i
	for j < len(w.covered) && w.covered[j][0] <= end {
		start, end = min(start, w.covered[j][0]), max(end, w.covered[j][1])
		j++
	}
	w.covered = append(w.covered[:i], append([][2]int64{{start, end}}, w.covered[j:]...)...)
}

func (w *sinkWriter) MaxLead() int64 { return w.sink.MaxLead }

func (w *sinkWriter) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	complete := w.spec.Size == 0 || (len(w.covered) == 1 && w.covered[0] == [2]int64{0, w.spec.Size})
	w.mu.Unlock()
	if !complete {
		return fmt.Errorf("fakes3: commit of %s before every byte was written", w.spec.DestPath)
	}
	s := w.sink
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.existing[w.spec.DestPath] || s.committed[w.spec.DestPath] {
		return engine.ErrDestinationExists
	}
	w.mu.Lock()
	w.ended = true
	w.mu.Unlock()
	s.committed[w.spec.DestPath] = true
	return nil
}

func (w *sinkWriter) Abort() error {
	w.mu.Lock()
	w.ended = true
	w.mu.Unlock()
	s := w.sink
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted[w.spec.DestPath] = true
	return nil
}
