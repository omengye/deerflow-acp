package assets

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Expand 8.3 names without following symlinks: confinement still applies to
// the requested path, and os.Root remains responsible for the actual open.
func resourcePathSpelling(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 32768)
	n, err := windows.GetLongPathName(p, &buf[0], uint32(len(buf)))
	if err != nil {
		return "", err
	}
	if n == 0 || n >= uint32(len(buf)) {
		return "", os.ErrInvalid
	}
	return windows.UTF16ToString(buf[:n]), nil
}

func openRead(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
