package skillauthor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestPackageRejectsOutputInsideShortPathSource(t *testing.T) {
	dir := fixture(t, "Read the report.")
	short := testutil.ShortPath(t, dir)
	long, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{short, long} {
		for _, parent := range []string{short, long} {
			output := filepath.Join(parent, "inside.skill")
			if _, err := Package(context.Background(), source, output); err == nil {
				t.Fatalf("accepted archive inside source: source=%s output=%s", source, output)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("rejected archive left output: %v", err)
			}
		}
	}
}
