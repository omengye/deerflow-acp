package assets_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

func TestToolOutputSnapshotSurvivesRestartAndStaysInSession(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	content := strings.Repeat("中文 and a complete tool result.\n", 4000)
	ref, err := f.assets.StoreToolOutput(ctx, f.session, content)
	if err != nil || ref.Kind != harness.AssetToolOutput || ref.Size != int64(len(content)) {
		t.Fatalf("snapshot=%+v err=%v", ref, err)
	}
	other, err := f.runtime.CreateSession(ctx, f.session.CWD, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.assets.ReadToolOutput(ctx, other, ref.ID, 0, 4096); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("other session read snapshot: %v", err)
	}
	wrongWorkspace := f.session
	wrongWorkspace.CWD = t.TempDir()
	if _, err := f.assets.ReadToolOutput(ctx, wrongWorkspace, ref.ID, 0, 4096); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("changed workspace read snapshot: %v", err)
	}
	if _, err := f.assets.ReadToolOutput(ctx, f.session, ref.ID, 1, 4096); err == nil {
		t.Fatal("middle-of-rune offset was accepted")
	}
	short, err := f.assets.ReadToolOutput(ctx, f.session, ref.ID, 0, 1)
	if err != nil || short.Text != "中" || short.NextOffset != 3 {
		t.Fatalf("small requested chunk did not make UTF-8 progress: %+v %v", short, err)
	}
	if err := f.assets.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := assets.NewStore(ctx, f.dir, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var complete strings.Builder
	for offset := int64(0); ; {
		part, err := reopened.ReadToolOutput(ctx, f.session, ref.ID, offset, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(part.Text) || len(part.Text) > 4096 || part.Offset != offset || part.NextOffset <= offset && !part.EOF || part.TotalBytes != int64(len(content)) {
			t.Fatalf("invalid bounded chunk: %+v", part)
		}
		complete.WriteString(part.Text)
		if part.EOF {
			break
		}
		offset = part.NextOffset
	}
	if complete.String() != content {
		t.Fatalf("reassembled snapshot differs: got %d bytes, want %d", complete.Len(), len(content))
	}
}
