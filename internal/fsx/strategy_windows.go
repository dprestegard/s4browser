//go:build windows

package fsx

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NewStrategy returns the named write strategy. lead is the MaxLead for the
// strategies that use one, or 0 for DefaultLead.
//
//   - sequential-lead: a plain file that grows as it is written. NTFS zero-fills
//     any gap below a write that starts past the valid data length, so the lead
//     bounds that cost.
//   - preallocate: the end of file is set to the final size first, which reserves
//     the clusters. The valid data length stays 0, so the gap rule is the same
//     and a lead is still needed.
//   - sparse: the file is marked sparse before the end of file is set, so the
//     unwritten range has no clusters and never needs zero-filling. Writes may
//     arrive in any order. Finish clears the attribute.
//   - valid-data: preallocate, then SetFileValidData, which skips zero-filling but
//     needs SeManageVolumePrivilege and exposes old disk contents until every
//     byte is overwritten. It fails with ErrUnsupportedStrategy unless the
//     process already holds the privilege.
func NewStrategy(name string, lead int64) (WriteStrategy, error) {
	if lead <= 0 {
		lead = DefaultLead
	}
	switch name {
	case StrategySequentialLead:
		return portableStrategy{name: name, lead: lead}, nil
	case StrategyPreallocate:
		return ntfsStrategy{name: name, lead: lead}, nil
	case StrategySparse:
		return ntfsStrategy{name: name, sparse: true}, nil
	case StrategyValidData:
		if err := enableManageVolume(); err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrUnsupportedStrategy, name, err)
		}
		return ntfsStrategy{name: name, validData: true}, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnsupportedStrategy, name)
}

// DefaultStrategy is the strategy used when the user has not chosen one.
func DefaultStrategy() WriteStrategy {
	s, _ := NewStrategy(StrategySparse, 0)
	return s
}

type ntfsStrategy struct {
	name      string
	lead      int64
	sparse    bool
	validData bool
}

func (s ntfsStrategy) Name() string   { return s.name }
func (s ntfsStrategy) MaxLead() int64 { return s.lead }

func (s ntfsStrategy) Create(path string, size int64) (WriteTarget, error) {
	if size < 0 {
		return nil, fmt.Errorf("fsx: negative size %d", size)
	}
	f, err := createExclusive(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (WriteTarget, error) {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	handle := windows.Handle(f.Fd())
	sparse, lead := s.sparse, s.lead
	if sparse {
		if err := setSparseFunc(handle, true); err != nil {
			// The volume has no sparse files (FAT, exFAT, some network shares). The
			// file then behaves like a preallocated one, so writes far past the valid
			// data length are zero-filled and the lead gate has to bound them.
			sparse, lead = false, DefaultLead
		}
	}
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			return fail(fmt.Errorf("fsx: set end of file: %w", err))
		}
		if s.validData {
			if err := windows.SetFileValidData(handle, size); err != nil {
				return fail(fmt.Errorf("fsx: SetFileValidData: %w", err))
			}
		}
	}
	return &ntfsTarget{fileTarget: fileTarget{f}, sparse: sparse, lead: lead}, nil
}

type ntfsTarget struct {
	fileTarget
	sparse bool
	lead   int64
}

// MaxLead reports the lead this file needs, which differs from its strategy's
// when a sparse file could not be made.
func (t *ntfsTarget) MaxLead() int64 { return t.lead }

// Finish clears the sparse attribute, so the finished file is an ordinary file.
// Every range has been written by then, so no hole remains. A failure is
// ignored: the file is complete and merely keeps the attribute, and failing a
// finished download over it would throw the data away.
func (t *ntfsTarget) Finish() error {
	if t.sparse {
		_ = setSparseFunc(windows.Handle(t.f.Fd()), false)
	}
	return nil
}

// setSparseFunc is replaced by tests to simulate a volume without sparse files.
var setSparseFunc = setSparse

// fileSetSparseBuffer is FILE_SET_SPARSE_BUFFER.
type fileSetSparseBuffer struct{ SetSparse uint8 }

func setSparse(h windows.Handle, on bool) error {
	buf := fileSetSparseBuffer{}
	if on {
		buf.SetSparse = 1
	}
	var returned uint32
	return windows.DeviceIoControl(h, windows.FSCTL_SET_SPARSE,
		(*byte)(unsafe.Pointer(&buf)), uint32(unsafe.Sizeof(buf)), nil, 0, &returned, nil)
}

var (
	volumeOnce sync.Once
	volumeErr  error
)

// enableManageVolume switches on SeManageVolumePrivilege if the token holds it.
// It never raises privileges: a standard or unelevated token does not hold it.
func enableManageVolume() error {
	volumeOnce.Do(func() {
		var token windows.Token
		if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
			volumeErr = err
			return
		}
		defer token.Close()
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeManageVolumePrivilege"), &luid); err != nil {
			volumeErr = err
			return
		}
		if !tokenHolds(token, luid) {
			volumeErr = errors.New("the process does not hold SeManageVolumePrivilege")
			return
		}
		privileges := windows.Tokenprivileges{PrivilegeCount: 1}
		privileges.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		volumeErr = windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil)
	})
	return volumeErr
}

// tokenHolds reports whether the token has the privilege, enabled or not.
// AdjustTokenPrivileges succeeds without an error when it does not, so the list
// is read instead.
func tokenHolds(token windows.Token, luid windows.LUID) bool {
	var size uint32
	_ = windows.GetTokenInformation(token, windows.TokenPrivileges, nil, 0, &size)
	if size == 0 {
		return false
	}
	buf := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, &buf[0], size, &size); err != nil {
		return false
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0]))
	for _, p := range unsafe.Slice(&privileges.Privileges[0], privileges.PrivilegeCount) {
		if p.Luid == luid {
			return true
		}
	}
	return false
}

// FreeSpace returns the bytes available to this user on the volume holding dir.
// Quotas are respected.
func FreeSpace(dir string) (int64, error) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(path, &available, &total, &free); err != nil {
		return 0, err
	}
	return int64(available), nil
}

type moveFinalizer struct{}

// RenameNoReplace is MoveFileExW with MOVEFILE_WRITE_THROUGH and without
// MOVEFILE_REPLACE_EXISTING. Go's os.Rename replaces the target, so it must not
// be used.
func (moveFinalizer) RenameNoReplace(tmp, final string) error {
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(final)
	if err != nil {
		return err
	}
	err = windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
	switch {
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS), errors.Is(err, windows.ERROR_FILE_EXISTS):
		return ErrDestinationExists
	case err != nil:
		return err
	}
	return nil
}

// NewFinalizer returns the platform's no-replace rename.
func NewFinalizer() Finalizer { return moveFinalizer{} }
