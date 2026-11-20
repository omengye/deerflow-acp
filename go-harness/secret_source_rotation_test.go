package deerflow

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

func TestImportSourceRotationAndRollback(t *testing.T) {
	for _, commit := range []bool{false, true} {
		root := t.TempDir()
		path := filepath.Join(root, "key")
		if err := os.WriteFile(path, []byte("first-fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		source, err := openWorkspaceImport(root, "key", true)
		if err != nil {
			t.Fatal(err)
		}
		b, err := source.read(secrets.KindFile)
		if err != nil || string(b) != "first-fixture" {
			t.Fatal("held source")
		}
		if runtime.GOOS == "windows" {
			if err = os.Rename(path, path+".old"); err == nil {
				t.Fatal("source was movable while importing")
			}
			if err = os.WriteFile(path, []byte("changed"), 0600); err == nil {
				t.Fatal("source accepted writes while importing")
			}
			if err = source.finish(commit); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if commit && !os.IsNotExist(err) || !commit && (err != nil || string(data) != "first-fixture") {
				t.Fatal("wrong disposition")
			}
		} else {
			if err = os.WriteFile(path, []byte("rotated-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			err = source.finish(commit)
			if commit && err != nil || !commit && err == nil {
				t.Fatal("unexpected recovery result", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "rotated-fixture" {
				t.Fatal("rotated source lost")
			}
		}
	}
}
