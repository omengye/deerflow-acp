//go:build !windows

package assets

import (
	"os"
	"path/filepath"
	"syscall"
)

func resourcePathSpelling(path string) (string, error) { return filepath.Clean(path), nil }

// Reject substituted FIFOs after Stat without blocking in Open.
func openRead(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
