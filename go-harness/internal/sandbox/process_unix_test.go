//go:build !windows

package sandbox

import (
	"os/exec"
	"syscall"
)

func hideTestProcess(cmd *exec.Cmd) {}
func processAlive(pid int) bool     { return syscall.Kill(pid, 0) == nil }
