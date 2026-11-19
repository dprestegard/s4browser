//go:build windows

package creds

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// processSuspendResume is PROCESS_SUSPEND_RESUME, which x/sys does not define.
const processSuspendResume = 0x0800

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// hiddenCommand runs command through cmd.exe, as the AWS SDK does, so .cmd and
// .bat helpers work. CREATE_NO_WINDOW gives cmd.exe a console with no window,
// and the helper it starts inherits that console.
//
// The command line is set by hand. Go would escape inner quotes as \", which
// cmd.exe does not understand, and a helper path with spaces must be quoted. /S
// makes cmd.exe remove exactly the outer pair of quotes, and /D skips AutoRun
// commands, which could write to stdout.
func hiddenCommand(ctx context.Context, command string) *exec.Cmd {
	shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	if os.Getenv("SystemRoot") == "" {
		shell = "cmd.exe"
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
		CmdLine:       `cmd.exe /D /S /C "` + command + `"`,
	}
	return cmd
}

// startContained starts cmd inside a job object, so cancelling ends the helper
// that cmd.exe started as well. Killing cmd.exe alone would leave a hung helper
// running, and credential recovery would add another one on every probe.
//
// The process starts suspended and is resumed only after it joins the job, so no
// child can escape. The job does not kill on close: a helper that finishes
// normally may leave a background agent running on purpose.
//
// The caller must call release after cmd.Wait returns.
func startContained(cmd *exec.Cmd) (release func(), err error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	release = func() { _ = windows.CloseHandle(job) }

	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	cmd.Cancel = func() error {
		_ = windows.TerminateJobObject(job, 1)
		return cmd.Process.Kill()
	}
	if err := cmd.Start(); err != nil {
		release()
		return nil, err
	}
	if err := joinAndResume(job, uint32(cmd.Process.Pid)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		release()
		return nil, err
	}
	return release, nil
}

func joinAndResume(job windows.Handle, pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|processSuspendResume, false, pid)
	if err != nil {
		return fmt.Errorf("open helper process: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fmt.Errorf("assign helper to job: %w", err)
	}
	if status, _, _ := ntResumeProcess.Call(uintptr(h)); status != 0 {
		return fmt.Errorf("resume helper: NTSTATUS 0x%x", status)
	}
	return nil
}
