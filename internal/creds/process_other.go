//go:build !windows

package creds

import (
	"context"
	"os/exec"
)

// hiddenCommand runs command through the shell, as the AWS SDK does. Other
// systems open no window for a child process.
func hiddenCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

// startContained starts cmd. The caller must call release after cmd.Wait returns.
func startContained(cmd *exec.Cmd) (release func(), err error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {}, nil
}
