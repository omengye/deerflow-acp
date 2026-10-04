package diagnostics

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonDiagnosticsBoundedAndNoRawErrors(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d.limit = 512
	for i := 0; i < 32; i++ {
		d.Record("startup.configuration", errors.New("api_key=never-persist-this-secret"))
	}
	d.Record("startup.ready", nil)
	status := d.Status()
	d.Close()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		t.Fatalf("rotations: %d %v", len(entries), err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil || len(data) > 512 || bytes.Contains(data, []byte("never-persist")) {
			t.Fatalf("invalid bounded log %s: %v", entry.Name(), err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if !json.Valid(line) {
				t.Fatal("incomplete record")
			}
		}
	}
	data, err := os.ReadFile(status["path"].(string))
	if err != nil || !bytes.Contains(data, []byte("startup.ready")) || status["enabled"] != true {
		t.Fatal("last lifecycle event missing")
	}
}
