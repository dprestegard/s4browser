//go:build !windows

package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// processLock is a flock on the lock file. The kernel releases it when the
// descriptor closes, including when the process is killed.
type processLock struct {
	file *os.File
}

func acquireProcessLock(path string) (*processLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: open lock file: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("journal: lock: %w", err)
	}
	return &processLock{file: file}, nil
}

func (l *processLock) release() error {
	return l.file.Close()
}

// replaceDurably renames from over to, then flushes the directory so the rename
// survives a power loss.
func replaceDurably(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
