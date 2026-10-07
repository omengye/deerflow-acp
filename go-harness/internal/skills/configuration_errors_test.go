package skills

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMissingConfiguredDirectoriesPreserveNotFoundCategory(t *testing.T) {
	root := t.TempDir()
	for _, export := range []bool{false, true} {
		var sources any = []any{map[string]any{"id": "managed", "root": "missing", "scope": "global"}}
		var exportRoot any
		field := "skills.managed_sources.0"
		if export {
			sources = nil
			exportRoot = "missing"
			field = "skills.export_root"
		}
		_, err := ResolveManagedSources(filepath.Join(root, "config.yaml"), sources, exportRoot)
		if !errors.Is(err, harness.ErrNotFound) || errors.Is(err, harness.ErrPermissionDenied) || !strings.Contains(err.Error(), field) {
			t.Fatalf("wrong missing directory error: %v", err)
		}
	}
}
