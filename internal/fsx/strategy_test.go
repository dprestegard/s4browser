package fsx

import (
	"bytes"
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

func testBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// finish runs what the engine's file sink does after the last write.
func finish(t *testing.T, target WriteTarget) {
	t.Helper()
	if f, ok := target.(Finisher); ok {
		if err := f.Finish(); err != nil {
			t.Fatal(err)
		}
	}
	if err := target.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStrategiesAssembleOutOfOrderWrites(t *testing.T) {
	const size = 3<<20 + 123
	want := testBytes(size, 1)
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			strategy, err := NewStrategy(name, 0)
			if errors.Is(err, ErrUnsupportedStrategy) {
				t.Skipf("not available here: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			if strategy.Name() != name {
				t.Fatalf("name %q", strategy.Name())
			}
			path := filepath.Join(t.TempDir(), "out.s4tmp")
			target, err := strategy.Create(path, size)
			if err != nil {
				t.Fatal(err)
			}
			// Highest chunk first, in uneven pieces, like parts finishing out of order.
			for end := size; end > 0; {
				start := max(end-700_001, 0)
				if n, err := target.WriteAt(want[start:end], int64(start)); err != nil || n != end-start {
					t.Fatal(n, err)
				}
				end = start
			}
			finish(t, target)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("assembled file differs")
			}
			if _, err := strategy.Create(path, size); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("Create over an existing file: %v", err)
			}
		})
	}
}

func TestStrategyLeads(t *testing.T) {
	cases := map[string]int64{StrategySequentialLead: 5 << 20, StrategyPreallocate: 5 << 20, StrategySparse: 0}
	for name, want := range cases {
		strategy, err := NewStrategy(name, 5<<20)
		if err != nil {
			t.Fatal(err)
		}
		if got := strategy.MaxLead(); got != want {
			t.Errorf("%s: MaxLead %d, want %d", name, got, want)
		}
	}
	if s, _ := NewStrategy(StrategyPreallocate, 0); s.MaxLead() != DefaultLead {
		t.Errorf("default lead %d", s.MaxLead())
	}
	if _, err := NewStrategy("nonsense", 0); !errors.Is(err, ErrUnsupportedStrategy) {
		t.Errorf("unknown strategy: %v", err)
	}
	if DefaultStrategy() == nil {
		t.Error("no default strategy")
	}
}

func TestZeroByteFile(t *testing.T) {
	for _, name := range Names() {
		strategy, err := NewStrategy(name, 0)
		if err != nil {
			continue
		}
		path := filepath.Join(t.TempDir(), "empty.s4tmp")
		target, err := strategy.Create(path, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		finish(t, target)
		if info, err := os.Stat(path); err != nil || info.Size() != 0 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
}

func TestRenameNoReplace(t *testing.T) {
	finalizers := map[string]Finalizer{"platform": NewFinalizer(), "link": linkRename{}}
	for name, f := range finalizers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			tmp, final := filepath.Join(dir, "a.s4tmp"), filepath.Join(dir, "a.mkv")
			if err := os.WriteFile(tmp, []byte("new"), 0o666); err != nil {
				t.Fatal(err)
			}
			if err := f.RenameNoReplace(tmp, final); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("temp file remains: %v", err)
			}
			if data, _ := os.ReadFile(final); string(data) != "new" {
				t.Fatalf("final holds %q", data)
			}

			// A second file must not replace the first, and neither may change.
			if err := os.WriteFile(tmp, []byte("newer"), 0o666); err != nil {
				t.Fatal(err)
			}
			if err := f.RenameNoReplace(tmp, final); !errors.Is(err, ErrDestinationExists) {
				t.Fatalf("got %v", err)
			}
			if data, _ := os.ReadFile(final); string(data) != "new" {
				t.Fatalf("existing file was replaced: %q", data)
			}
			if data, _ := os.ReadFile(tmp); string(data) != "newer" {
				t.Fatalf("temp file was touched: %q", data)
			}
		})
	}
}

type linkRename struct{}

func (linkRename) RenameNoReplace(tmp, final string) error { return renameNoReplaceLink(tmp, final) }

func TestFreeSpace(t *testing.T) {
	free, err := FreeSpace(t.TempDir())
	if err != nil || free <= 0 {
		t.Fatalf("free space %d, %v", free, err)
	}
	if _, err := FreeSpace(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing directory must be an error")
	}
}
