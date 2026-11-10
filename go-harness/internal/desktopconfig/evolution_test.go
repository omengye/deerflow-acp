package desktopconfig

import (
	"os"
	"path/filepath"

	"testing"
)

func reviewDocument(t *testing.T) (Options, map[string]any) {
	t.Helper()
	options := setup(t)
	run(t, options, "init", nil)
	doc := run(t, options, "snapshot", nil)
	root := str(readObject(doc, "paths"), "skills", "")
	if err := os.MkdirAll(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	return options, doc
}
