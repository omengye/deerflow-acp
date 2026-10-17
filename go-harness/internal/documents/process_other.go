//go:build !windows

package documents

import "os/exec"

func hideProcess(*exec.Cmd) {}
