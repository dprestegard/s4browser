//go:build windows

package journal

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// processLock is a byte-range lock on the lock file. Windows releases it when the
// handle closes, including when the process is killed.
type processLock struct {
	handle windows.Handle
}

func acquireProcessLock(path string) (*processLock, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("journal: lock path: %w", err)
	}
	handle, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("journal: open lock file: %w", err)
	}
	overlapped := new(windows.Overlapped)
	err = windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err != nil {
		_ = windows.CloseHandle(handle)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("journal: lock: %w", err)
	}
	return &processLock{handle: handle}, nil
}

func (l *processLock) release() error {
	return windows.CloseHandle(l.handle)
}

// replaceDurably renames from over to with write-through, so the rename has
// reached the disk when it returns. Antivirus and indexers can hold the target
// open for a moment, which is retried briefly.
func replaceDurably(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	const attempts = 20
	for attempt := 1; ; attempt++ {
		err = windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		transient := errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
		if err == nil || !transient || attempt == attempts {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
