package engine

import (
	"context"
	"fmt"
	"os"
)

// FileSource is the production Source. It reads local files through one shared
// handle per job. os.File.ReadAt is safe for concurrent use.
type FileSource struct{}

var _ Source = FileSource{}

// Open implements Source. Directories are rejected.
func (FileSource) Open(ctx context.Context, path string) (SourceHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return fileHandle{file}, nil
}

type fileHandle struct{ file *os.File }

func (h fileHandle) ReadAt(p []byte, off int64) (int, error) { return h.file.ReadAt(p, off) }

func (h fileHandle) Stat() (SourceStat, error) {
	info, err := h.file.Stat()
	if err != nil {
		return SourceStat{}, err
	}
	return SourceStat{Size: info.Size(), ModTime: info.ModTime()}, nil
}

func (h fileHandle) Close() error { return h.file.Close() }
