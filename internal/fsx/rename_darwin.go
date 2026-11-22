package fsx

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames tmp to final unless final exists.
//
// On APFS and HFS+ this is renamex_np with RENAME_EXCL, which is atomic. A
// volume that does not support it falls back to the hard-link rename, also
// atomic. exFAT and FAT32 (camera cards, many external drives) support neither,
// so there the last resort checks that final is missing and then renames. That
// leaves a short window in which another program could create final and have it
// replaced; the app itself never writes final names outside this call.
func renameNoReplace(tmp, final string) error {
	err := unix.RenamexNp(tmp, final, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return ErrDestinationExists
	case !unsupported(err):
		return err
	}
	err = renameNoReplaceLink(tmp, final)
	if err == nil || !unsupported(err) {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return ErrDestinationExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(tmp, final)
}

// unsupported reports whether err means the volume cannot do the operation.
func unsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM)
}
