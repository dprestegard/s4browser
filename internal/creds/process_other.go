//go:build !windows

package creds

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// hiddenCommand runs command through the shell, as the AWS SDK does. Other
// systems open no window for a child process.
func hiddenCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if runtime.GOOS == "darwin" {
		cmd.Env = withHelperPath(os.Environ())
	}
	return cmd
}

// macHelperDirs are where Homebrew and most installers put command-line tools.
// An app started from Finder gets only launchd's PATH (/usr/bin:/bin:/usr/sbin:
// /sbin), so a helper such as "aws-vault" or "granted" would not be found.
var macHelperDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// withHelperPath returns env with macHelperDirs appended to PATH, skipping any
// already on it. The user's own order is kept.
func withHelperPath(env []string) []string {
	out := make([]string, 0, len(env)+1)
	path, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path, found = v, true
			continue
		}
		out = append(out, kv)
	}
	if !found {
		path = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	have := map[string]bool{}
	for _, d := range strings.Split(path, ":") {
		have[d] = true
	}
	for _, d := range macHelperDirs {
		if !have[d] {
			path += ":" + d
		}
	}
	return append(out, "PATH="+path)
}

// startContained starts cmd. The caller must call release after cmd.Wait returns.
func startContained(cmd *exec.Cmd) (release func(), err error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {}, nil
}
