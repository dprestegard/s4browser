package journal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrClosed is returned by a FileStore after Close.
var ErrClosed = errors.New("journal: store closed")

// FileStore is the production Store: one JSON snapshot, replaced atomically on
// every change, guarded by an exclusive process lock.
//
// Holding the lock is what stops a second app instance from reading this journal
// and aborting the first instance's live uploads. The operating system releases
// the lock when the process dies, so a crash never leaves the journal locked.
type FileStore struct {
	dir  string
	lock *processLock

	// write serializes snapshot writes. It is a channel so that a caller can stop
	// waiting when its context ends.
	write chan struct{}

	mu       sync.RWMutex
	entries  map[string]Entry
	warnings []string
	closed   bool
}

var _ Store = (*FileStore)(nil)

// OpenFile takes the process lock in dir and loads the journal.
//
//   - Another live process holding the lock returns ErrLocked.
//   - A leftover journal.json.tmp is an unfinished write, so it is deleted.
//   - A corrupt journal is moved to a journal.corrupt-<timestamp>.json name and the
//     store starts empty. Warnings reports the move.
//   - A journal from a newer build returns ErrUnsupportedVersion. The file is left
//     alone and the lock is released.
func OpenFile(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: create directory: %w", err)
	}
	lock, err := acquireProcessLock(filepath.Join(dir, LockFileName))
	if err != nil {
		return nil, err
	}
	store := &FileStore{dir: dir, lock: lock, write: make(chan struct{}, 1), entries: make(map[string]Entry)}
	if err := store.load(); err != nil {
		_ = lock.release()
		return nil, err
	}
	return store, nil
}

func (s *FileStore) load() error {
	if err := os.Remove(filepath.Join(s.dir, TempFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("journal: remove stale snapshot: %w", err)
	}
	path := filepath.Join(s.dir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("journal: read: %w", err)
	}
	entries, err := Decode(data)
	switch {
	case err == nil:
		for _, entry := range entries {
			s.entries[entry.ID] = entry
		}
		return nil
	case errors.Is(err, ErrCorrupt):
		moved := CorruptPrefix + time.Now().UTC().Format("20060102T150405.000000000Z") + ".json"
		if renameErr := os.Rename(path, filepath.Join(s.dir, moved)); renameErr != nil {
			return fmt.Errorf("journal: set aside corrupt file: %w", renameErr)
		}
		s.warnings = append(s.warnings, fmt.Sprintf("The transfer journal was corrupt and was moved to %s. Unfinished uploads from the last session may remain on S3.", moved))
		return nil
	default:
		return err
	}
}

// Warnings returns problems found while opening the journal, for the user.
func (s *FileStore) Warnings() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.warnings)
}

// Put implements Store.
func (s *FileStore) Put(ctx context.Context, entry Entry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	return s.commit(ctx, func(next map[string]Entry) bool {
		next[entry.ID] = entry
		return true
	})
}

// Delete implements Store.
func (s *FileStore) Delete(ctx context.Context, id string) error {
	return s.commit(ctx, func(next map[string]Entry) bool {
		if _, ok := next[id]; !ok {
			return false
		}
		delete(next, id)
		return true
	})
}

// commit applies change to a copy of the entries, makes the copy durable and only
// then publishes it. A failed write therefore leaves memory and disk in agreement.
func (s *FileStore) commit(ctx context.Context, change func(map[string]Entry) bool) error {
	select {
	case s.write <- struct{}{}:
		defer func() { <-s.write }()
	case <-ctx.Done():
		return context.Cause(ctx)
	}

	s.mu.RLock()
	closed := s.closed
	next := maps.Clone(s.entries)
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if !change(next) {
		return nil
	}
	if err := s.writeSnapshot(next); err != nil {
		return err
	}
	s.mu.Lock()
	s.entries = next
	s.mu.Unlock()
	return nil
}

// writeSnapshot follows the design's atomic write rule: write the whole snapshot
// to the temp file, flush it, close it, then rename it over the journal.
func (s *FileStore) writeSnapshot(entries map[string]Entry) error {
	data, err := Encode(slices.Collect(maps.Values(entries)))
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, TempFileName)
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("journal: create snapshot: %w", err)
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("journal: write snapshot: %w", err)
	}
	if err := replaceDurably(tmp, filepath.Join(s.dir, FileName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("journal: commit snapshot: %w", err)
	}
	return nil
}

// List implements Store.
func (s *FileStore) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := slices.Collect(maps.Values(s.entries))
	slices.SortFunc(list, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })
	return list
}

// Close waits for any write in progress and releases the process lock.
func (s *FileStore) Close() error {
	s.write <- struct{}{}
	defer func() { <-s.write }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.release()
}
