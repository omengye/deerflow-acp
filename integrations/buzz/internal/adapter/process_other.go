//go:build !windows

package adapter

import "os/exec"

func hideCommand(c *exec.Cmd)           {}
func executableName(name string) string { return name }
