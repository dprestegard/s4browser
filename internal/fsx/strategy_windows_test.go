//go:build windows

package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var compressedFileSize = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

func attributes(t *testing.T, path string) uint32 {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	return attrs
}

func allocated(t *testing.T, path string) int64 {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	var high uint32
	r, _, err := compressedFileSize.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&high)))
	if uint32(r) == 0xFFFFFFFF && err != windows.ERROR_SUCCESS {
		t.Fatal(err)
	}
	return int64(high)<<32 | int64(uint32(r))
}

// TestSparseStrategyAvoidsZeroFill proves the property the strategy exists for:
// a file with its end of file set but nothing written has no clusters, and the
// sparse attribute is gone once the file is finished.
func TestSparseStrategyAvoidsZeroFill(t *testing.T) {
	const size = 256 << 20
	strategy, err := NewStrategy(StrategySparse, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sparse.s4tmp")
	target, err := strategy.Create(path, size)
	if err != nil {
		t.Fatal(err)
	}
	if attributes(t, path)&windows.FILE_ATTRIBUTE_SPARSE_FILE == 0 {
		t.Fatal("the file is not sparse")
	}
	// A write far past the start must not allocate the gap below it.
	if _, err := target.WriteAt(make([]byte, 1<<20), size-(1<<20)); err != nil {
		t.Fatal(err)
	}
	if got := allocated(t, path); got > 8<<20 {
		t.Fatalf("%d bytes allocated for a 1 MiB write, so the gap was zero-filled", got)
	}
	if _, err := target.WriteAt(make([]byte, size-(1<<20)), 0); err != nil {
		t.Fatal(err)
	}
	finish(t, target)
	if attributes(t, path)&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		t.Fatal("the sparse attribute was not cleared")
	}
}

// TestSparseFallsBackWhereTheVolumeHasNoSparseFiles simulates exFAT: the file is
// still created, and it asks for a lead instead.
func TestSparseFallsBackWhereTheVolumeHasNoSparseFiles(t *testing.T) {
	old := setSparseFunc
	setSparseFunc = func(windows.Handle, bool) error { return windows.ERROR_INVALID_FUNCTION }
	defer func() { setSparseFunc = old }()

	strategy, err := NewStrategy(StrategySparse, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strategy.MaxLead() != 0 {
		t.Fatalf("the strategy itself needs no lead, got %d", strategy.MaxLead())
	}
	path := filepath.Join(t.TempDir(), "fallback.s4tmp")
	target, err := strategy.Create(path, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	leader, ok := target.(Leader)
	if !ok || leader.MaxLead() != DefaultLead {
		t.Fatalf("the fallback file must ask for DefaultLead: %v", target)
	}
	if attributes(t, path)&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		t.Fatal("the file is sparse")
	}
	finish(t, target)
}

func TestPreallocateSetsTheLength(t *testing.T) {
	strategy, err := NewStrategy(StrategyPreallocate, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pre.s4tmp")
	target, err := strategy.Create(path, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if attributes(t, path)&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		t.Fatal("a preallocated file must not be sparse")
	}
}

// TestValidDataNeedsThePrivilege documents that valid-data is used only when the
// process already holds SeManageVolumePrivilege, and never raises privileges.
func TestValidDataNeedsThePrivilege(t *testing.T) {
	strategy, err := NewStrategy(StrategyValidData, 0)
	if err != nil {
		if !errors.Is(err, ErrUnsupportedStrategy) {
			t.Fatal(err)
		}
		t.Logf("not available, as expected for an unelevated process: %v", err)
		return
	}
	if strategy.MaxLead() != 0 {
		t.Fatalf("valid-data needs no lead, got %d", strategy.MaxLead())
	}
}

func TestIsDiskFull(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "write", Path: "x", Err: windows.ERROR_DISK_FULL},
		&os.PathError{Op: "write", Path: "x", Err: windows.ERROR_HANDLE_DISK_FULL},
		ErrDiskFull,
	} {
		if !IsDiskFull(err) {
			t.Errorf("%v is a full disk", err)
		}
	}
	for _, err := range []error{nil, windows.ERROR_ACCESS_DENIED, errors.New("disk full")} {
		if IsDiskFull(err) {
			t.Errorf("%v is not a full disk", err)
		}
	}
}

func TestVolumeOf(t *testing.T) {
	dir := t.TempDir()
	a, err := VolumeOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := VolumeOf(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || a != b || a != strings.ToLower(a) {
		t.Fatalf("volumes %q and %q", a, b)
	}
	if want := strings.ToLower(filepath.VolumeName(dir)) + `\`; a != want {
		t.Logf("volume %q, drive %q (a mounted folder?)", a, want)
	}
}
