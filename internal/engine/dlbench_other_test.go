//go:build !windows

package engine_test

func processPeakWorkingSet() int64 { return 0 }

func extentCount(string) int { return -1 }
