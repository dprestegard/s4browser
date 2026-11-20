package fsx

import "errors"

// ErrDiskFull means a write failed because the volume, or the user's quota on
// it, is full. IsDiskFull recognises the platform's errors for it.
var ErrDiskFull = errors.New("fsx: the drive is full")

// Preallocator is an optional interface of a WriteTarget. Preallocated reports
// whether Create already took the file's clusters from the volume, so that the
// bytes still to be written need no further free space. A target without it is
// assumed to allocate as it is written.
type Preallocator interface {
	Preallocated() bool
}

// IsDiskFull reports whether err is a full volume or an exceeded quota.
func IsDiskFull(err error) bool {
	return err != nil && (errors.Is(err, ErrDiskFull) || isDiskFull(err))
}
