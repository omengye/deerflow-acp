package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// NormalizeWorkspace resolves the directory once at attachment time. File
// operations must additionally use os.Root to resist symlink replacement races.
func NormalizeWorkspace(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: cwd must be an absolute directory", harness.ErrInvalidInput)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("%w: cwd must resolve to an existing directory", harness.ErrInvalidInput)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("%w: cwd cannot be accessed", harness.ErrInvalidInput)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: cwd is not a directory", harness.ErrInvalidInput)
	}
	return real, nil
}

func SameWorkspace(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
