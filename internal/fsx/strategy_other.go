//go:build !windows

package fsx

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// NewStrategy returns the named write strategy. lead is the MaxLead for the
// strategies that use one, or 0 for DefaultLead. On Unix filesystems every
// strategy is the portable one: truncating to the final size makes a sparse file
// without zero-filling, so no lead is needed for sparse. valid-data is NTFS-only.
func NewStrategy(name string, lead int64) (WriteStrategy, error) {
	if lead <= 0 {
		lead = DefaultLead
	}
	switch name {
	case StrategySequentialLead:
		return portableStrategy{name: name, lead: lead}, nil
	case StrategyPreallocate:
		return portableStrategy{name: name, extend: true, lead: lead}, nil
	case StrategySparse:
		return portableStrategy{name: name, extend: true}, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnsupportedStrategy, name)
}

// DefaultStrategy is the strategy used when the user has not chosen one.
func DefaultStrategy() WriteStrategy {
	s, _ := NewStrategy(StrategySparse, 0)
	return s
}

// FreeSpace returns the bytes available to this user on the volume holding dir.
func FreeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// isDiskFull matches ENOSPC and EDQUOT.
func isDiskFull(err error) bool {
	return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT)
}

// VolumeOf names the filesystem holding dir by its device number. Downloads to
// the same filesystem share its free space.
func VolumeOf(dir string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return "", err
	}
	return fmt.Sprint(st.Dev), nil
}

type linkFinalizer struct{}

func (linkFinalizer) RenameNoReplace(tmp, final string) error { return renameNoReplaceLink(tmp, final) }

// NewFinalizer returns the platform's no-replace rename.
func NewFinalizer() Finalizer { return linkFinalizer{} }
