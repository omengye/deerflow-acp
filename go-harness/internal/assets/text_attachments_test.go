package assets_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

func TestTextAttachmentSnapshotReadIsolationAndRestart(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	contents := "你好 " + strings.Repeat("value,42\n", 1000)
	in := f.accept(t, []harness.Content{{Type: "attachment", Name: "notes.csv", Text: contents}})
	if in[0].Text != "" || in[0].Asset == nil {
		t.Fatal("attachment body entered history")
	}
	id := in[0].Asset.ID
	list, err := f.assets.ListAttachments(ctx, f.session, "")
	if err != nil || len(list.Attachments) != 1 || list.Attachments[0].ID != id {
		t.Fatalf("list: %+v %v", list, err)
	}
	var got strings.Builder
	var offset int64
	for {
		chunk, err := f.assets.ReadAttachment(ctx, f.session, id, offset, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Text) > 4096 {
			t.Fatal("unbounded read")
		}
		got.WriteString(chunk.Text)
		if chunk.EOF {
			break
		}
		offset = chunk.NextOffset
	}
	if got.String() != contents {
		t.Fatal("attachment changed")
	}
	other, err := f.runtime.CreateSession(ctx, f.session.CWD, "vision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.assets.ReadAttachment(ctx, other, id, 0, 4096); err == nil {
		t.Fatal("cross-session read")
	}
	if listed, listErr := f.assets.ListAttachments(ctx, other, ""); listErr != nil || len(listed.Attachments) != 0 {
		t.Fatal("cross-session listing", listed, listErr)
	}
	wrongWorkspace := f.session
	wrongWorkspace.CWD = t.TempDir()
	if _, err = f.assets.ReadAttachment(ctx, wrongWorkspace, id, 0, 4096); err == nil {
		t.Fatal("workspace grant ignored")
	}
	if _, err = f.assets.ReadAttachment(ctx, f.session, filepath.Join(f.dir, id+".blob"), 0, 4096); err == nil {
		t.Fatal("path accepted as ID")
	}
	if _, err = f.assets.ReadAttachment(ctx, f.session, id, 1, 4096); err == nil {
		t.Fatal("invalid UTF-8 offset")
	}
	if err = f.assets.Close(); err != nil {
		t.Fatal(err)
	}
	f.assets, err = assets.NewStore(ctx, f.dir, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.assets.Close() })
	chunk, err := f.assets.ReadAttachment(ctx, f.session, id, 0, 4096)
	if err != nil || !strings.HasPrefix(chunk.Text, "你好") {
		t.Fatalf("restart: %+v %v", chunk, err)
	}
	if err = os.WriteFile(filepath.Join(f.dir, id+".blob"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.assets.ReadAttachment(ctx, f.session, id, 0, 4096); err == nil {
		t.Fatal("tampered snapshot read")
	}
}

func TestTextAttachmentListingPaginatesAndHidesUncommittedInput(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	p, err := f.assets.Prepare(ctx, f.session, []harness.Content{{Type: "attachment", Name: "pending.txt", Text: "uncommitted"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.assets.ReadAttachment(ctx, f.session, p.Input[0].Asset.ID, 0, 4096); err == nil {
		t.Error("uncommitted input read")
	}
	if page, err := f.assets.ListAttachments(ctx, f.session, ""); err != nil || len(page.Attachments) != 0 {
		t.Error("uncommitted input listed", page, err)
	}
	if err = p.Finish(false); err != nil {
		t.Fatal(err)
	}
	for batch := 0; batch < 5; batch++ {
		input := make([]harness.Content, 8)
		for i := range input {
			input[i] = harness.Content{Type: "attachment", Name: "a.txt", Text: "bounded"}
		}
		f.accept(t, input)
	}
	first, err := f.assets.ListAttachments(ctx, f.session, "")
	if err != nil || len(first.Attachments) != 32 || first.NextCursor == "" {
		t.Fatal("first page", first, err)
	}
	second, err := f.assets.ListAttachments(ctx, f.session, first.NextCursor)
	if err != nil || len(second.Attachments) != 8 || second.NextCursor != "" {
		t.Fatal("second page", second, err)
	}
	seen := map[string]bool{}
	for _, ref := range append(first.Attachments, second.Attachments...) {
		if seen[ref.ID] {
			t.Fatal("duplicate page entry")
		}
		seen[ref.ID] = true
	}
}

func TestTextAttachmentRejectsUnsupportedAndOversized(t *testing.T) {
	f := newAssetFixture(t)
	for _, c := range []harness.Content{
		{Type: "attachment", Name: "file.pdf", Text: "data"},
		{Type: "attachment", Name: "../file.txt", Text: "data"},
		{Type: "attachment", Name: "file.txt", Text: "binary\x00"},
		{Type: "attachment", Name: "file.txt", Text: string([]byte{0xff})},
		{Type: "attachment", Name: "file.txt", Text: strings.Repeat("x", harness.MaxTextAttachmentBytes+1)},
		{Type: "attachment", Name: "file.txt", URI: "file:///outside", Text: "data"},
	} {
		if p, err := f.assets.Prepare(context.Background(), f.session, []harness.Content{c}); err == nil {
			p.Finish(false)
			t.Fatalf("accepted %s", c.Name)
		}
	}
	for _, count := range []int{5, 9} {
		input := make([]harness.Content, count)
		for i := range input {
			input[i] = harness.Content{Type: "attachment", Name: "a.txt", Text: strings.Repeat("a", harness.MaxTextAttachmentBytes)}
		}
		if p, err := f.assets.Prepare(context.Background(), f.session, input); err == nil {
			p.Finish(false)
			t.Fatal("aggregate cap ignored")
		}
	}
}
