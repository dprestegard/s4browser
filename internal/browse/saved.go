package browse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const (
	savedFileName    = "buckets.json"
	savedFileVersion = 1
)

// savedBucket is a bucket the user added by name.
type savedBucket struct {
	Name   string `json:"name"`
	Region string `json:"region,omitempty"`
}

type savedState struct {
	Version int `json:"version"`
	// Profiles maps a profile ID to its added buckets.
	Profiles map[string][]savedBucket `json:"profiles"`
}

// savedBuckets is buckets.json. An empty dir keeps the list in memory only.
type savedBuckets struct {
	path  string
	mu    sync.Mutex
	state savedState
}

func openSaved(dir string) (*savedBuckets, error) {
	s := &savedBuckets{state: savedState{Version: savedFileVersion, Profiles: map[string][]savedBucket{}}}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, savedFileName)
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("browse: read saved buckets: %w", err)
	}
	// A damaged file is an error and stays in place, so the list is not lost.
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("browse: %s is damaged: %w", s.path, err)
	}
	if s.state.Version != savedFileVersion {
		return nil, fmt.Errorf("browse: %s has unsupported version %d", s.path, s.state.Version)
	}
	if s.state.Profiles == nil {
		s.state.Profiles = map[string][]savedBucket{}
	}
	return s, nil
}

func (s *savedBuckets) list(profile string) []savedBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.state.Profiles[profile])
}

func (s *savedBuckets) add(profile string, b savedBucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneLocked()
	list := next.Profiles[profile]
	if i := slices.IndexFunc(list, func(x savedBucket) bool { return x.Name == b.Name }); i >= 0 {
		list[i] = b
	} else {
		list = append(list, b)
	}
	slices.SortFunc(list, func(a, b savedBucket) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	next.Profiles[profile] = list
	return s.saveLocked(next)
}

func (s *savedBuckets) remove(profile, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneLocked()
	list := next.Profiles[profile]
	i := slices.IndexFunc(list, func(x savedBucket) bool { return x.Name == name })
	if i < 0 {
		return fmt.Errorf("bucket %q was not added by name", name)
	}
	list = slices.Delete(list, i, i+1)
	if len(list) == 0 {
		delete(next.Profiles, profile)
	} else {
		next.Profiles[profile] = list
	}
	return s.saveLocked(next)
}

func (s *savedBuckets) cloneLocked() savedState {
	next := savedState{Version: savedFileVersion, Profiles: make(map[string][]savedBucket, len(s.state.Profiles))}
	for k, v := range s.state.Profiles {
		next.Profiles[k] = slices.Clone(v)
	}
	return next
}

// saveLocked writes next atomically and then adopts it, so a failed write
// leaves the list as it was.
func (s *savedBuckets) saveLocked(next savedState) error {
	if s.path == "" {
		s.state = next
		return nil
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("browse: encode saved buckets: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("browse: save buckets: %w", err)
	}
	tmp, err := os.CreateTemp(dir, savedFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("browse: save buckets: %w", err)
	}
	defer os.Remove(tmp.Name()) // fails harmlessly after the rename
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		return fmt.Errorf("browse: save buckets: %w", err)
	}
	s.state = next
	return nil
}
