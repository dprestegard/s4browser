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
