//go:build !windows

package assets

import (
	"os"
	"syscall"
)

// Reject substituted FIFOs after Stat without blocking in Open.
func openRead(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
