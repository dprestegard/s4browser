package fsx

import (
	"errors"
	"io/fs"
	"os"
)

// renameNoReplaceLink is the portable no-replace rename: a hard link fails if the
// target exists, and the old name is removed afterwards. Both names are in one
// directory, so the link is atomic. If removing tmp fails, both names exist and
// the data is safe under final.
func renameNoReplaceLink(tmp, final string) error {
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrDestinationExists
		}
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return nil
}
