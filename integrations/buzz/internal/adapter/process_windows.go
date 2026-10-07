//go:build windows

package adapter

import (
	"os/exec"
	"syscall"
)

func hideCommand(c *exec.Cmd)           { c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} }
func executableName(name string) string { return name + ".exe" }
