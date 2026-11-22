package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNoReplaceDarwin(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "a.s4tmp")
	final := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(tmp, final); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("existing final: %v, want ErrDestinationExists", err)
	}
	if b, _ := os.ReadFile(final); string(b) != "old" {
		t.Fatalf("final was replaced: %q", b)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("tmp is gone after a refused rename: %v", err)
	}

	os.Remove(final)
	if err := renameNoReplace(tmp, final); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(final); string(b) != "new" {
		t.Fatalf("final: %q", b)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp still exists: %v", err)
	}
}
