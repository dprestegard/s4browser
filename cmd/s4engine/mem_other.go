//go:build !windows

package main

import (
	"runtime"
	"syscall"
)

// peakWorkingSet returns the peak resident set size of this process, or 0 if it
// cannot be read.
func peakWorkingSet() uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil || ru.Maxrss < 0 {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return uint64(ru.Maxrss) // bytes
	}
	return uint64(ru.Maxrss) * 1024 // kilobytes
}
