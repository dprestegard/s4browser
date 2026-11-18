package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"s4browser/internal/fsx"
)

// FreeSpaceReserve is kept free on the volume beyond the object itself, so a
// download cannot take the last bytes of a drive.
const FreeSpaceReserve = 64 * MiB

// FileSink is the production Sink. It writes a temp file beside the final path
// through an fsx.WriteStrategy and moves it into place with a no-replace rename.
type FileSink struct {
	strategy  fsx.WriteStrategy
	finalizer fsx.Finalizer
	// FreeSpace returns the bytes available in a directory's volume. Tests replace it.
	FreeSpace func(dir string) (int64, error)
}

var _ Sink = (*FileSink)(nil)

// NewFileSink returns a sink using strategy for every download.
func NewFileSink(strategy fsx.WriteStrategy) *FileSink {
	return &FileSink{strategy: strategy, finalizer: fsx.NewFinalizer(), FreeSpace: fsx.FreeSpace}
}

// Prepare implements Sink. It checks the folder, the collision and the free
// space, then creates the temp file at fsx.TempPath(DestPath, JobID). Any file it
// created is removed again if it fails.
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
	available, err := s.FreeSpace(dir)
	if err != nil {
		return nil, fmt.Errorf("free space of %s: %w", dir, err)
	}
	if need := spec.Size + FreeSpaceReserve; available < need {
		return nil, fmt.Errorf("%w: the download needs %s (%s plus a %s reserve) but only %s is free on the drive holding %s",
			ErrInsufficientSpace, FormatBytes(need), FormatBytes(spec.Size), FormatBytes(FreeSpaceReserve), FormatBytes(available), dir)
	}
	temp := fsx.TempPath(spec.DestPath, string(spec.JobID))
	target, err := s.strategy.Create(temp, spec.Size)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", temp, err)
	}
	return &fileWriter{sink: s, spec: spec, temp: temp, target: target}, nil
}

type fileWriter struct {
	sink   *FileSink
	spec   SinkSpec
	temp   string
	target fsx.WriteTarget

	mu        sync.Mutex
	closed    bool // The target handle is closed.
	committed bool
}

func (w *fileWriter) WriteAt(p []byte, off int64) (int, error) { return w.target.WriteAt(p, off) }

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
