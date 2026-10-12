package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestNormalizeWorkspacePinsLongPathBeforeComparison(t *testing.T) {
	long := testutil.CanonicalTempDir(t)
	short := testutil.ShortPath(t, long)
	pinned, err := NormalizeWorkspace(short)
	if err != nil || !SameWorkspace(pinned, long) {
		t.Fatalf("normalized short workspace=%q want=%q err=%v", pinned, long, err)
	}
	redirect := filepath.Join(testutil.CanonicalTempDir(t), "redirected-workspace")
	if err := os.Symlink(long, redirect); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := NormalizeWorkspace(redirect)
	if err != nil {
		t.Fatal(err)
	}
	if SameWorkspace(redirect, resolved) {
		t.Fatal("workspace comparison followed a replacement symlink")
	}
}
