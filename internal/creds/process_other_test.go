//go:build !windows

package creds

import (
	"slices"
	"testing"
)

func TestHelperPathAddsHomebrew(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{[]string{"HOME=/x", "PATH=/usr/bin:/bin"}, "PATH=/usr/bin:/bin:/opt/homebrew/bin:/usr/local/bin"},
		{[]string{"PATH=/opt/homebrew/bin:/usr/bin"}, "PATH=/opt/homebrew/bin:/usr/bin:/usr/local/bin"},
		{[]string{"HOME=/x"}, "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin"},
	} {
		got := withHelperPath(tc.env)
		if !slices.Contains(got, tc.want) {
			t.Errorf("%v: got %v, want %s", tc.env, got, tc.want)
		}
		n := 0
		for _, kv := range got {
			if len(kv) > 5 && kv[:5] == "PATH=" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%v: %d PATH entries in %v", tc.env, n, got)
		}
	}
}
