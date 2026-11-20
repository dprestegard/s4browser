package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"s4browser/internal/fsx"
)

// FreeSpaceReserve is kept free on the volume beyond the object itself, so a
// download cannot take the last bytes of a drive.
const FreeSpaceReserve = 64 * MiB

// FileSink is the production Sink. It writes a temp file beside the final path
// through an fsx.WriteStrategy and moves it into place with a no-replace rename.
//
// Free space is shared by every download to a volume. The sink keeps the bytes
// each open temp file still has to take from its volume, and admits a new
// download only if free space covers it, the others' remainder and the reserve.
type FileSink struct {
	strategy  fsx.WriteStrategy
	finalizer fsx.Finalizer
	// FreeSpace returns the bytes available in a directory's volume. Tests replace it.
	FreeSpace func(dir string) (int64, error)
	// Volume names the volume holding a directory. Tests replace it.
	Volume func(dir string) (string, error)

	// mu serialises admission, so two downloads cannot both count the same
	// free space, and guards open.
	mu   sync.Mutex
	open map[*fileWriter]struct{}
}

var _ Sink = (*FileSink)(nil)

// NewFileSink returns a sink using strategy for every download.
func NewFileSink(strategy fsx.WriteStrategy) *FileSink {
	return &FileSink{strategy: strategy, finalizer: fsx.NewFinalizer(), FreeSpace: fsx.FreeSpace, Volume: fsx.VolumeOf}
}

// Reserved returns the bytes that open temp files on volume still have to
// take. It is for tests and diagnostics.
func (s *FileSink) Reserved(volume string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reservedLocked(volume)
}

func (s *FileSink) reservedLocked(volume string) int64 {
	var total int64
	for w := range s.open {
		if w.volume == volume {
			total += w.remaining()
		}
	}
	return total
}

// release stops counting w against its volume. It is called once the temp file
// is renamed or removed, or removal was given up.
func (s *FileSink) release(w *fileWriter) {
	s.mu.Lock()
	delete(s.open, w)
	s.mu.Unlock()
}

// Prepare implements Sink. It checks the folder, the collision and the free
// space, then creates the temp file at fsx.TempPath(DestPath, JobID). A leftover
// temp file of the same job, from a run whose cleanup could not remove it, is
// removed first. Any file it created is removed again if it fails.
func (s *FileSink) Prepare(ctx context.Context, spec SinkSpec) (SinkWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if !filepath.IsAbs(spec.DestPath) {
		return nil, fmt.Errorf("%w: %q is not an absolute path", ErrUnsafePath, spec.DestPath)
	}
	dir := filepath.Dir(spec.DestPath)
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		return nil, fmt.Errorf("destination folder %s: %w", dir, err)
	case !info.IsDir():
		return nil, fmt.Errorf("destination folder %s is not a folder", dir)
	}
	if _, err := os.Lstat(spec.DestPath); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrDestinationExists, spec.DestPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	volume, err := s.Volume(dir)
	if err != nil {
		return nil, fmt.Errorf("volume of %s: %w", dir, err)
	}
	temp := fsx.TempPath(spec.DestPath, string(spec.JobID))
	// The name belongs to this job alone, so a file there is its own leftover.
	if err := os.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove the temporary file left by an earlier attempt, %s: %w", temp, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	available, err := s.FreeSpace(dir)
	if err != nil {
		return nil, fmt.Errorf("free space of %s: %w", dir, err)
	}
	reserved := s.reservedLocked(volume)
	if need := spec.Size + FreeSpaceReserve + reserved; available < need {
		others := ""
		if reserved > 0 {
			others = fmt.Sprintf(", plus %s still to be written by other downloads to the same drive", FormatBytes(reserved))
		}
		return nil, fmt.Errorf("%w: the download needs %s (%s plus a %s reserve%s) but only %s is free on the drive holding %s",
			ErrInsufficientSpace, FormatBytes(need), FormatBytes(spec.Size), FormatBytes(FreeSpaceReserve), others, FormatBytes(available), dir)
	}
	target, err := s.strategy.Create(temp, spec.Size)
	if err != nil {
		if fsx.IsDiskFull(err) {
			return nil, fmt.Errorf("%w: create %s: %w", ErrInsufficientSpace, temp, err)
		}
		return nil, fmt.Errorf("create %s: %w", temp, err)
	}
	w := &fileWriter{sink: s, spec: spec, temp: temp, target: target, volume: volume}
	if p, ok := target.(fsx.Preallocator); ok && p.Preallocated() {
		w.preallocated = true
	}
	if s.open == nil {
		s.open = make(map[*fileWriter]struct{})
	}
	s.open[w] = struct{}{}
	return w, nil
}

type fileWriter struct {
	sink   *FileSink
	spec   SinkSpec
	temp   string
	target fsx.WriteTarget
	volume string
	// preallocated means Create took every cluster, so nothing is reserved.
	preallocated bool
	// written counts bytes written. A download writes each byte once.
	written atomic.Int64

	mu        sync.Mutex
	closed    bool // The target handle is closed.
	committed bool
	released  bool
}

// remaining is the free space this file may still take.
func (w *fileWriter) remaining() int64 {
	if w.preallocated {
		return 0
	}
	return max(w.spec.Size-w.written.Load(), 0)
}

// WriteAt implements SinkWriter. A full drive is reported as ErrDiskFull.
func (w *fileWriter) WriteAt(p []byte, off int64) (int, error) {
	n, err := w.target.WriteAt(p, off)
	w.written.Add(int64(n))
	if err != nil && fsx.IsDiskFull(err) && !errors.Is(err, ErrDiskFull) {
		err = fmt.Errorf("%w: %w", ErrDiskFull, err)
	}
	return n, err
}

// releaseLocked stops counting the file against its volume. w.mu is held.
func (w *fileWriter) releaseLocked() {
	if !w.released {
		w.released = true
		w.sink.release(w)
	}
}

// MaxLead asks the file first, because a strategy may have had to fall back.
func (w *fileWriter) MaxLead() int64 {
	if l, ok := w.target.(fsx.Leader); ok {
		return l.MaxLead()
	}
	return w.sink.strategy.MaxLead()
}

// Commit implements SinkWriter. The order is Finish, Sync, Close, size check and
// a no-replace rename. Cancellation is honoured between steps, because a sync
// cannot be interrupted.
func (w *fileWriter) Commit(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return nil
	}
	if w.closed {
		return errors.New("engine: commit after abort")
	}
	steps := []func() error{
		func() error {
			if f, ok := w.target.(fsx.Finisher); ok {
				return f.Finish()
			}
			return nil
		},
		w.target.Sync,
		func() error {
			w.closed = true
			return w.target.Close()
		},
		func() error {
			info, err := os.Stat(w.temp)
			if err != nil {
				return err
			}
			if info.Size() != w.spec.Size {
				return fmt.Errorf("temp file is %d bytes, expected %d", info.Size(), w.spec.Size)
			}
			return nil
		},
		func() error { return w.sink.finalizer.RenameNoReplace(w.temp, w.spec.DestPath) },
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		if err := step(); err != nil {
			return err
		}
	}
	w.committed = true
	w.releaseLocked()
	return nil
}

// Abort implements SinkWriter. It is safe to call more than once and after a
// successful Commit, when it does nothing.
func (w *fileWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return nil
	}
	// Whatever happens to the file, it takes no more space from now on: what it
	// holds already is out of the free space the next admission reads.
	w.releaseLocked()
	var first error
	if !w.closed {
		w.closed = true
		first = w.target.Close()
	}
	if err := os.Remove(w.temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = first // A failed Close does not matter once the file is gone.
	return nil
}

// FormatBytes renders a size for messages, in binary units.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
