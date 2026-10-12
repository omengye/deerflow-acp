package desktopconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyImportOperationsAreUnsupported(t *testing.T) {
	options := setup(t)
	run(t, options, "snapshot", nil)
	config, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	archiveDir := filepath.Join(options.UserData, "data", "legacy-history")
	if err := os.MkdirAll(archiveDir, 0700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(archiveDir, strings.Repeat("a", 64)+".json")
	archive := []byte(`{"read_only":true,"entries":[]}`)
	if err := os.WriteFile(archivePath, archive, 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{
		"legacy-agent-preview", "legacy-history-import", "legacy-history-list", "legacy-history-get",
	} {
		t.Run(operation, func(t *testing.T) {
			value, err := Run(options, operation, json.RawMessage(`{}`))
			if err == nil || !strings.Contains(err.Error(), "unsupported configuration operation") || value != nil {
				t.Fatalf("removed operation returned value=%v, err=%v", value, err)
			}
			current, err := os.ReadFile(options.Config)
			if err != nil || string(current) != string(config) {
				t.Fatalf("removed operation changed configuration: %v", err)
			}
			current, err = os.ReadFile(archivePath)
			if err != nil || string(current) != string(archive) {
				t.Fatalf("removed operation changed an existing archive: %v", err)
			}
		})
	}
}
