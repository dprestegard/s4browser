// Package fsx holds local filesystem contracts: download write strategies,
// temporary file naming and no-replace finalization. Windows-specific
// implementations live in files with a _windows.go suffix, added in stage 0.5.
package fsx

import (
	"errors"
	"io"
	"path/filepath"
	"regexp"
)

// Sentinel errors, re-exported by internal/engine.
var (
	// ErrDestinationExists means the final path is already occupied.
	ErrDestinationExists = errors.New("fsx: destination already exists")
	// ErrInsufficientSpace means the volume cannot hold the file.
	ErrInsufficientSpace = errors.New("fsx: insufficient disk space")
	// ErrUnsafePath means a path escapes its root or is invalid on this OS.
	ErrUnsafePath = errors.New("fsx: unsafe path")
)

// Strategy names compared by stage 0.5.
const (
	// StrategySequentialLead extends the file as written, with a small MaxLead,
	// so that NTFS never zero-fills a long gap.
	StrategySequentialLead = "sequential-lead"
	// StrategySparse marks the file sparse and writes in any order.
	StrategySparse = "sparse"
	// StrategyPreallocate sets the end of file up front and uses a MaxLead.
	StrategyPreallocate = "preallocate"
	// StrategyValidData calls SetFileValidData, which needs
	// SeManageVolumePrivilege and is used only when the process holds it.
	StrategyValidData = "valid-data"
)

// WriteTarget is an open temporary file. WriteAt is safe for concurrent use on
// non-overlapping ranges.
type WriteTarget interface {
	io.WriterAt
	// Sync flushes file data to stable storage.
	Sync() error
	Close() error
}

// WriteStrategy creates download temp files in one specific way.
type WriteStrategy interface {
	// Name returns one of the Strategy constants.
	Name() string
	// Create makes a new file at path, failing if it exists. size is the final
	// length, and the file must have exactly that length after the last write.
	Create(path string, size int64) (WriteTarget, error)
	// MaxLead is the furthest a write may start past the lowest unfinished
	// offset. 0 means unlimited.
	MaxLead() int64
}

// Finalizer moves a finished temp file to its final name.
type Finalizer interface {
	// RenameNoReplace atomically renames tmp to final on the same volume. It
	// returns ErrDestinationExists and leaves both files untouched if final
	// exists. On Windows this is MoveFileExW with MOVEFILE_WRITE_THROUGH and
	// without MOVEFILE_REPLACE_EXISTING.
	RenameNoReplace(tmp, final string) error
}

// TempSuffix ends every download temp file name.
const TempSuffix = ".s4tmp"

const tempPrefix = "~s4-"

var tempNamePattern = regexp.MustCompile(`^~s4-[0-9a-f]{16}\.s4tmp$`)

// TempPath returns the temp file for a download: the final file's directory,
// which guarantees an atomic rename, and a fixed-length name, which cannot
// exceed the filename limit however long the final name is. jobID is the
// engine's 16-hex-character job ID.
func TempPath(finalPath, jobID string) string {
	return filepath.Join(filepath.Dir(finalPath), tempPrefix+jobID+TempSuffix)
}

// IsTempPath reports whether path has the exact temp file name form. Launch
// cleanup must check this before deleting anything named by the journal.
func IsTempPath(path string) bool {
	return tempNamePattern.MatchString(filepath.Base(path))
}
