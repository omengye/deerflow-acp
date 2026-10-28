package desktopconfig

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadYAMLWithRevisionBindsParsedDataToExactSourceBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var previousRevision string
	for _, body := range []string{
		"name: original\nvalue: 7\n",
		"# same values, different source bytes\r\nname: original\r\nvalue: 7\r\n",
		"name: 'original'\nvalue:    7\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		data, revision, err := readYAMLWithRevision(path)
		if err != nil {
			t.Fatal(err)
		}
		if data["name"] != "original" || data["value"] != 7 {
			t.Fatal("source values were not parsed")
		}
		if revision != fmt.Sprintf("%x", sha256.Sum256([]byte(body))) {
			t.Fatal("revision was not hashed from exact source bytes")
		}
		if revision == previousRevision {
			t.Fatal("equivalent YAML syntax reused a previous byte revision")
		}
		previousRevision = revision
		if err := writeYAMLAtomic(path, map[string]any{"name": "replacement", "value": 8}); err != nil {
			t.Fatal(err)
		}
		current, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if current == revision || data["name"] != "original" {
			t.Fatal("later atomic replacement changed the observed data/revision pair")
		}
	}
}

func TestReadYAMLWithRevisionRejectsInvalidOrOversizedDataWithoutAToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, body := range []string{
		"broken: [",
		"null\n",
		"[]\n",
		"#" + strings.Repeat("x", 2<<20),
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		data, revision, err := readYAMLWithRevision(path)
		if err == nil || data != nil || revision != "" {
			t.Fatal("invalid YAML received a usable document or revision")
		}
	}
	if data, revision, err := readYAMLWithRevision(path + ".missing"); err == nil || data != nil || revision != "" {
		t.Fatal("missing YAML received a usable document or revision")
	}
}
