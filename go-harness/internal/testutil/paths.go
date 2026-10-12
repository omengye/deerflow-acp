// Package testutil provides fixtures shared by internal regression tests.
package testutil

import (
	"path/filepath"
	"testing"
)

// CanonicalTempDir mirrors session attachment for fixtures that bypass the
// public service. Windows TEMP may contain 8.3 aliases; macOS may use symlinks.
func CanonicalTempDir(t testing.TB) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
