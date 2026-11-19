package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
}

// parseSize parses a byte count such as 5368709120, 64MiB or 10GB.
func parseSize(s string) (int64, error) {
	num, unit := splitNumber(s)
	mult, ok := sizeUnits[strings.ToLower(unit)]
	if !ok || num == "" {
		return 0, fmt.Errorf("invalid size %q (use bytes, KiB, MiB, GiB, TiB, KB, MB, GB or TB)", s)
	}
	return scale(s, num, mult)
}

// parseRate parses a bandwidth cap into bytes per second. It accepts a size
// per second ("50MiB", "625MB") or bits per second ("5Gbit", "500Mbit").
func parseRate(s string) (int64, error) {
	num, unit := splitNumber(strings.TrimSuffix(s, "/s"))
	lower := strings.ToLower(unit)
	if bits, ok := strings.CutSuffix(lower, "bit"); ok {
		mult, ok := map[string]float64{"": 1, "k": 1e3, "m": 1e6, "g": 1e9}[bits]
		if !ok || num == "" {
			return 0, fmt.Errorf("invalid rate %q", s)
		}
		return scale(s, num, mult/8)
	}
	n, err := parseSize(num + unit)
	if err != nil {
		return 0, fmt.Errorf("invalid rate %q (use e.g. 50MiB, 625MB or 5Gbit)", s)
	}
	return n, nil
}

func splitNumber(s string) (num, unit string) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	return s[:i], strings.TrimSpace(s[i:])
}

func scale(orig, num string, mult float64) (int64, error) {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f*mult > math.MaxInt64/2 {
		return 0, fmt.Errorf("invalid number in %q", orig)
	}
	return int64(math.Round(f * mult)), nil
}
