//go:build !windows

package tools

import (
	"os"
	"syscall"
)

// Do not let a FIFO substituted for a file block the run before f.Stat can
// reject it. The flag has no effect for ordinary files and directories.
func openRead(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
