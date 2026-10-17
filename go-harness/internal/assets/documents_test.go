package assets_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/documents"
)

func TestDocumentAttachmentExtractionIsolationAndAnalysis(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	part, _ := w.Create("word/document.xml")
	_, _ = part.Write([]byte(`<document><p><t>Document evidence</t></p></document>`))
	_ = w.Close()
	input := f.accept(t, []harness.Content{{Type: "attachment", Name: "report.docx", MimeType: documents.MIME("report.docx"), Data: base64.StdEncoding.EncodeToString(archive.Bytes())}, {Type: "attachment", Name: "data.csv", Text: "key,value\na,10\nb,30\n"}})
	chunk, err := f.assets.ReadAttachment(ctx, f.session, input[0].Asset.ID, 0, 4096)
	if err != nil || !strings.Contains(chunk.Text, "Document evidence") {
		t.Fatalf("%+v %v", chunk, err)
	}
	var cached int
	if err = f.db.DB().QueryRow(`SELECT COUNT(*) FROM harness_document_text WHERE asset_id=?`, input[0].Asset.ID).Scan(&cached); err != nil || cached != 1 {
		t.Fatal("extraction cache missing", cached, err)
	}
	stats, err := f.assets.AnalyzeAttachment(ctx, f.session, input[1].Asset.ID, true)
	if err != nil || stats == nil {
		t.Fatal(stats, err)
	}
	other := f.session
	other.ID = "other"
	if _, err = f.assets.AnalyzeAttachment(ctx, other, input[1].Asset.ID, true); err == nil {
		t.Fatal("cross-session analysis")
	}
	for _, c := range []harness.Content{{Type: "attachment", Name: "a.docx", MimeType: "application/pdf", Data: "YWJj"}, {Type: "attachment", Name: "../a.pdf", MimeType: "application/pdf", Data: "YWJj"}, {Type: "attachment", Name: "a.pdf", MimeType: "application/pdf", Data: "not base64"}, {Type: "attachment", Name: "a.pdf", MimeType: "application/pdf", Text: "mixed", Data: "YWJj"}} {
		p, err := f.assets.Prepare(ctx, f.session, []harness.Content{c})
		if err == nil {
			p.Finish(false)
			t.Fatal("accepted invalid binary attachment", c.Name)
		}
	}
	if err = os.WriteFile(filepath.Join(f.dir, input[0].Asset.ID+".blob"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.assets.ReadAttachment(ctx, f.session, input[0].Asset.ID, 0, 4096); err == nil {
		t.Fatal("cache bypassed original snapshot verification")
	}
}
