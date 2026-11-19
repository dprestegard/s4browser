package queue

import (
	"context"
	"slices"
	"sync"

	"s4browser/internal/journal"
)

// memoryJournal is the journal used when journal.json cannot be used. Nothing
// is written to disk, so it cannot damage the file it replaces.
type memoryJournal struct {
	mu      sync.Mutex
	entries map[string]journal.Entry
}

var _ journal.Store = (*memoryJournal)(nil)

func newMemoryJournal() *memoryJournal { return &memoryJournal{entries: map[string]journal.Entry{}} }

func (m *memoryJournal) Put(_ context.Context, e journal.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[e.ID] = e
	return nil
}

func (m *memoryJournal) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, id)
	return nil
}

func (m *memoryJournal) List() []journal.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]journal.Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b journal.Entry) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

func (m *memoryJournal) Close() error { return nil }
