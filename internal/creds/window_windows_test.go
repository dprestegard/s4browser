//go:build windows

package creds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// reportWindow runs inside the helper. It records whether the console the helper
// is attached to has a visible window.
func reportWindow(path string) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	hwnd, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	visible := uintptr(0)
	if hwnd != 0 {
		visible, _, _ = user32.NewProc("IsWindowVisible").Call(hwnd)
	}
	_ = os.WriteFile(path, fmt.Appendf(nil, "hwnd=%t visible=%t", hwnd != 0, visible != 0), 0o600)
}

func TestHiddenCommandFlags(t *testing.T) {
	cmd := hiddenCommand(t.Context(), `"C:\Program Files\x.exe" --a "b c"`)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 || !cmd.SysProcAttr.HideWindow {
		t.Fatalf("%+v", cmd.SysProcAttr)
	}
	want := `cmd.exe /D /S /C ""C:\Program Files\x.exe" --a "b c""`
	if cmd.SysProcAttr.CmdLine != want {
		t.Fatalf("command line %s", cmd.SysProcAttr.CmdLine)
	}
}

// A cancelled helper must really stop, including the process cmd.exe started.
func TestCancelEndsTheHelperProcess(t *testing.T) {
	t.Setenv("S4_CRED_HELPER", "hang")
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("S4_HELPER_PID", pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	command := helperExe(t)
	go func() {
		_, err := NewProcessProvider(command).Retrieve(ctx)
		done <- err
	}()

	var pid int
	for deadline := time.Now().Add(30 * time.Second); pid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the helper did not start")
		}
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(string(data))
		}
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Retrieve did not return after cancel")
	}
	if ev, err := windows.WaitForSingleObject(h, 10_000); err != nil || ev != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(h, 1)
		t.Fatalf("the helper is still running after cancel: %d %v", ev, err)
	}
}

// The real vault, with a throwaway service name and fake values. It writes to
// Credential Manager, so it runs only on request.
func TestSystemVaultRoundTrip(t *testing.T) {
	if os.Getenv("S4_VAULT_TEST") == "" {
		t.Skip("set S4_VAULT_TEST=1 to use the real Credential Manager")
	}
	v := osVault{service: fmt.Sprintf("s4browser-test-%d", time.Now().UnixNano())}
	t.Cleanup(func() { _ = v.Delete(vaultUser("roundtrip")) })
	if _, err := readKeys(v, "roundtrip"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("%v", err)
	}
	if err := writeKeys(v, "roundtrip", fakeAccess, fakeSecret); err != nil {
		t.Fatal(err)
	}
	keys, err := readKeys(v, "roundtrip")
	if err != nil || keys.AccessKeyID != fakeAccess || keys.SecretAccessKey != fakeSecret {
		t.Fatalf("%q %v", keys.AccessKeyID, err)
	}
	if err := v.Delete(vaultUser("roundtrip")); err != nil {
		t.Fatal(err)
	}
	if err := v.Delete(vaultUser("roundtrip")); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("%v", err)
	}
}

// runFromGUIApp builds testdata/guiprobe as a real GUI-subsystem program, which
// has no console of its own, and has it run the helper in the given mode.
func runFromGUIApp(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "guiprobe.exe")
	build := exec.Command("go", "build", "-ldflags=-H=windowsgui", "-o", probe, "./testdata/guiprobe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	report := filepath.Join(dir, "window.txt")
	result := filepath.Join(dir, "result.txt")
	command := helperExe(t)
	if mode == "sdk" {
		// The SDK's provider cannot run a quoted command, so use a path that
		// needs no quotes.
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command = exe
	}
	run := exec.Command(probe, mode, command, result)
	run.Env = append(os.Environ(), "S4_CRED_HELPER=window", "S4_WINDOW_REPORT="+report)
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
	case <-time.After(30 * time.Second):
		_ = run.Process.Kill()
		t.Fatal("the probe did not finish")
	}
	got, err := os.ReadFile(result)
	if err != nil || strings.TrimSpace(string(got)) != procAccess {
		t.Fatalf("probe result %q: %v", got, err)
	}
	window, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the helper wrote no report: %v", err)
	}
	return string(window)
}

func TestHelperHasNoVisibleWindowFromGUIApp(t *testing.T) {
	window := runFromGUIApp(t, "hidden")
	t.Log(window)
	if strings.Contains(window, "visible=true") {
		t.Fatalf("the helper has a visible console window: %s", window)
	}
}

// TestSDKProviderControl shows what the hidden provider prevents. It opens a real
// console window on the desktop, so it runs only on request.
func TestSDKProviderControl(t *testing.T) {
	if os.Getenv("S4_GUI_CONTROL") == "" {
		t.Skip("set S4_GUI_CONTROL=1: this test flashes a console window")
	}
	t.Log(runFromGUIApp(t, "sdk"))
}
