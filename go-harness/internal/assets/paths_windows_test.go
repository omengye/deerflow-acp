package assets_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestResourceShortPathAliasesPreserveWorkspaceBoundary(t *testing.T) {
	f := newAssetFixture(t)
	path := filepath.Join(f.session.CWD, "long attachment filename.txt")
	if err := os.WriteFile(path, []byte("inside workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	short := testutil.ShortPath(t, path)
	out := f.accept(t, []harness.Content{{Type: "resource_link", URI: fileURI(short)}})
	data, err := f.assets.Resolve(context.Background(), f.session.ID, *out[0].Asset)
	if err != nil || string(data) != "inside workspace" {
		t.Fatalf("short path snapshot=%q err=%v", data, err)
	}
	outside := filepath.Join(testutil.CanonicalTempDir(t), "long private filename.txt")
	if err := os.WriteFile(outside, []byte("outside workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{outside, testutil.ShortPath(t, outside)} {
		p, err := f.assets.Prepare(context.Background(), f.session, []harness.Content{{Type: "resource_link", URI: fileURI(candidate)}})
		if err == nil {
			p.Finish(false)
			t.Fatal("outside resource accepted through an alias")
		}
	}
}

func TestResourceShortPathDoesNotFollowOutsideSymlink(t *testing.T) {
	f := newAssetFixture(t)
	outside := filepath.Join(testutil.CanonicalTempDir(t), "private.txt")
	if err := os.WriteFile(outside, []byte("outside workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.session.CWD, "long symbolic link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p, err := f.assets.Prepare(context.Background(), f.session, []harness.Content{{Type: "resource_link", URI: fileURI(testutil.ShortPath(t, link))}})
	if err == nil {
		p.Finish(false)
		t.Fatal("short symlink escaped the workspace")
	}
}
