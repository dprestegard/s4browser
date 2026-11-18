package engine

import (
	"errors"
	"testing"
)

func TestDefaultSettingsAreValid(t *testing.T) {
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsValidateRejects(t *testing.T) {
	tests := map[string]func(*Settings){
		"no files":        func(s *Settings) { s.MaxActiveFiles = 0 },
		"too many files":  func(s *Settings) { s.MaxActiveFiles = MaxActiveFilesLimit + 1 },
		"no parts":        func(s *Settings) { s.MaxPartsPerFile = 0 },
		"too many parts":  func(s *Settings) { s.MaxPartsPerFile = MaxPartsPerFileLimit + 1 },
		"no global parts": func(s *Settings) { s.MaxActiveParts = 0 },
		"too many global": func(s *Settings) { s.MaxActiveParts = MaxActivePartsLimit + 1 },
		"small part":      func(s *Settings) { s.PartSize = MinPartSize - 1 },
		"large part":      func(s *Settings) { s.PartSize = MaxPartSize + 1 },
		"negative up":     func(s *Settings) { s.UploadBytesPerSec = -1 },
		"negative down":   func(s *Settings) { s.DownloadBytesPerSec = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := DefaultSettings()
			mutate(&s)
			if err := s.Validate(); !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestMemoryBoundAtDefaults(t *testing.T) {
	got := MemoryBound(DefaultSettings(), 1000)
	want := 16*MiB + 36*384*KiB + 32*MiB + 4*10_000*96 + 1000*2*KiB
	if got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
	if got > 100*MiB {
		t.Fatalf("default bound %d MiB exceeds the 100 MiB design target", got/MiB)
	}
}

func TestMemoryBoundIgnoresPartSize(t *testing.T) {
	small, large := DefaultSettings(), DefaultSettings()
	small.PartSize = MinPartSize
	large.PartSize = MaxPartSize
	if MemoryBound(small, 1) != MemoryBound(large, 1) {
		t.Fatal("memory bound must not scale with part size")
	}
}
