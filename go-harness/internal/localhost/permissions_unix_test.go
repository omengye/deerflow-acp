//go:build !windows

package localhost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointPrivateModes(t *testing.T) {
	dir := t.TempDir()
	_, _ = startHost(t, Config{RuntimeDir: dir})
	for name, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, EndpointFilename): 0600, filepath.Join(dir, "daemon.lock"): 0600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s has mode %o, want %o", name, info.Mode().Perm(), mode)
		}
	}
}
