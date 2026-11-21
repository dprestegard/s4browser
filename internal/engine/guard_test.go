package engine

import (
	"errors"
	"testing"
)

func TestTestPrefixGuard(t *testing.T) {
	guard, err := NewTestPrefixGuard("run-42")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		key     string
		allowed bool
	}{
		{"s4browser-test/run-42/file.bin", true},
		{"s4browser-test/run-42/dir/sub/file.bin", true},
		{"s4browser-test/run-42/name..with.dots", true},
		{"s4browser-test/run-42/", false},
		{"s4browser-test/run-42", false},
		{"s4browser-test/run-421/file.bin", false},
		{"s4browser-test/run-4/file.bin", false},
		{"s4browser-test/other/file.bin", false},
		{"s4browser-test/file.bin", false},
		{"production/file.bin", false},
		{"", false},
		{"/s4browser-test/run-42/file.bin", false},
		{"S4BROWSER-TEST/run-42/file.bin", false},
		{"s4browser-test/run-42/../other/file.bin", false},
		{"s4browser-test/run-42/a/../../escape", false},
		{"s4browser-test/run-42/./file.bin", false},
		{"s4browser-test/run-42//file.bin", false},
		// A folder placeholder: one trailing slash, nothing unsafe before it.
		{"s4browser-test/run-42/dir/", true},
		{"s4browser-test/run-42/dir/sub/", true},
		{"s4browser-test/run-42//", false},
		{"s4browser-test/run-42/dir//", false},
		{"s4browser-test/run-42/../", false},
		{"s4browser-test/run-42/dir/./", false},
		{`s4browser-test/run-42/..\..\escape`, false},
		{"s4browser-test/run-42/nul\x00byte", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			err := guard.CheckWrite(tt.key)
			if tt.allowed && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tt.allowed && !errors.Is(err, ErrWriteRefused) {
				t.Fatalf("got %v, want ErrWriteRefused", err)
			}
		})
	}
}

func TestNewTestPrefixGuardRejectsBadRunIDs(t *testing.T) {
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "-leading", "a.b", "has space", string(long)} {
		if _, err := NewTestPrefixGuard(id); !errors.Is(err, ErrWriteRefused) {
			t.Errorf("run ID %q: got %v", id, err)
		}
	}
}

func TestNewPrefixGuard(t *testing.T) {
	for _, bad := range []string{"", "/", "upload-test", "upload-test/", "/upload-test/run/", "upload-test//", "upload-test/../",
		"upload-test/./", `upload-test\run/`, "upload-test/run", "a/\x00/"} {
		if _, err := NewPrefixGuard(bad); !errors.Is(err, ErrWriteRefused) {
			t.Errorf("%q was accepted", bad)
		}
	}
	g, err := NewPrefixGuard("upload-test/s4browser-43/")
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"upload-test/s4browser-43/a.bin", "upload-test/s4browser-43/reel/", "upload-test/s4browser-43/x/y.mov"} {
		if err := g.CheckWrite(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"upload-test/a.bin", "upload-test/s4browser-43/", "upload-test/s4browser-430/a", "upload-test/s4browser-43/../a",
		"other/s4browser-43/a", "upload-test/s4browser-43//a"} {
		if err := g.CheckWrite(bad); !errors.Is(err, ErrWriteRefused) {
			t.Errorf("%q was allowed", bad)
		}
	}
}
