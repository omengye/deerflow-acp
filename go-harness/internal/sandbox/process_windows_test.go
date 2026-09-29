package sandbox

import (
	"context"
	"os/exec"
	"syscall"
	"testing"

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
