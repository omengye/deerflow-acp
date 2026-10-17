//go:build !windows

package channels

import "os/exec"

func hideProcess(cmd *exec.Cmd) {}
