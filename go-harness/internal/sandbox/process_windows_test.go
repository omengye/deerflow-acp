package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"golang.org/x/sys/windows"
)

func hideTestProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}

func TestWindowsJobResourceLimits(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MemoryBytes = 256 << 20
	cfg.Limits.MaxProcesses = 2
	cfg.Limits.CPUPercent = 50
	b := newTestBackend(t, cfg, t.TempDir())
	s := waitTask(t, b, launchTask(t, b, helperRequest(t, "echo")).ID)
	if !s.TerminationConfirmed || s.ExitCode == nil || *s.ExitCode != 7 {
		t.Fatalf("Job limits: %+v", s)
	}
	if _, err := b.Poll(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if s.State != harness.CommandFailed {
		t.Fatal(s.State)
	}
}

// Windows App Execution Aliases can launch through a broker outside the
// command's Job. Opt in on hosts with Python Manager to exercise that path.
func TestWindowsPythonAliasCleanup(t *testing.T) {
	if os.Getenv("DEERFLOW_TEST_PYTHON_ALIAS") != "1" {
		t.Skip("set DEERFLOW_TEST_PYTHON_ALIAS=1 on a Python Manager host")
	}
	for _, mode := range []string{"cancel", "timeout", "parent_exit"} {
		t.Run(mode, func(t *testing.T) { testWindowsPythonAliasCleanup(t, mode) })
	}
}

func testWindowsPythonAliasCleanup(t *testing.T, mode string) {
	cwd := t.TempDir()
	fixture := "import os,pathlib,time\npathlib.Path('python.pid').write_text(str(os.getpid()))\nprint('ready',flush=True)\ntime.sleep(30)\n"
	if err := os.WriteFile(filepath.Join(cwd, "fixture.py"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	b := newTestBackend(t, harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true, Environment: map[string]string{
		"LOCALAPPDATA": os.Getenv("LOCALAPPDATA"), "APPDATA": os.Getenv("APPDATA"), "USERPROFILE": os.Getenv("USERPROFILE"),
		"PYTHON_MANAGER_AUTOMATIC_INSTALL": "false",
	}, Limits: harness.CommandLimits{Timeout: 30 * time.Second}}, cwd)
	request := harness.CommandRequest{Script: "python fixture.py"}
	expected := harness.CommandCancelled
	if mode == "timeout" {
		request.Timeout = 5 * time.Second
		expected = harness.CommandTimedOut
	} else if mode == "parent_exit" {
		request.Script = `Start-Process -FilePath python -ArgumentList 'fixture.py' -NoNewWindow; while (-not (Test-Path -LiteralPath 'python.pid')) { Start-Sleep -Milliseconds 20 }; exit 0`
		expected = harness.CommandCompleted
	}
	s := launchTask(t, b, request)
	var pid int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(cwd, "python.pid"))
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("Python alias did not start: %+v", waitTask(t, b, s.ID))
	}
	// Retain the exact process handle so a failing test never leaves the
	// sleeping fixture behind and cannot terminate a reused PID.
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil && mode != "parent_exit" {
		t.Fatal(err)
	}
	if err == nil {
		defer windows.CloseHandle(handle)
		defer windows.TerminateProcess(handle, 1)
	}
	if mode == "cancel" {
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s = waitTask(t, b, s.ID)
	if !s.TerminationConfirmed || s.State != expected {
		t.Fatalf("Python alias cancellation: %+v", s)
	}
	if processAlive(pid) {
		t.Fatal("Python alias descendant survived command cancellation")
	}
}

func TestWindowsDescendantLifetimeRejectsReusedParents(t *testing.T) {
	for _, tc := range []struct {
		child, parent, exited uint64
		want                  bool
	}{
		{90, 100, 0, false},
		{110, 100, 120, true},
		{121, 100, 120, false},
		{110, 100, 0, true},
	} {
		if got := withinParentLifetime(tc.child, tc.parent, tc.exited); got != tc.want {
			t.Errorf("lifetime %+v: got %v", tc, got)
		}
	}
}

func TestWindowsCleanupLeavesUnrelatedProcessAlive(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	unrelated := exec.Command(exe, "-test.run=^TestCommandHelper$", "--", "sleep")
	unrelated.Env = append(os.Environ(), "DEERFLOW_SANDBOX_HELPER=1")
	hideTestProcess(unrelated)
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	b := newTestBackend(t, testConfig(t), t.TempDir())
	s := launchTask(t, b, helperRequest(t, "sleep"))
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if result := waitTask(t, b, s.ID); !result.TerminationConfirmed {
		t.Fatalf("command cleanup: %+v", result)
	}
	if !processAlive(unrelated.Process.Pid) {
		t.Fatal("cleanup terminated an unrelated process")
	}
}
