//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type unixTree struct{ pid int }

func newProcessTree(cmd *exec.Cmd, _ harness.CommandLimits) (processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &unixTree{}, nil
}
func (t *unixTree) afterStart(cmd *exec.Cmd) error { t.pid = cmd.Process.Pid; return nil }
func (t *unixTree) terminate() error {
	if t.pid == 0 {
		return nil
	}
	if err := syscall.Kill(-t.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(cleanupTimeout)
	for {
		err := syscall.Kill(-t.pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process group termination was not confirmed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (t *unixTree) close() error   { return nil }
func defaultContainerUser() string { return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) }
