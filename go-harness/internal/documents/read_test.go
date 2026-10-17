package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func office(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestDocumentDOCXExtractAndBounds(t *testing.T) {
	data := office(t, map[string]string{"word/document.xml": `<document><body><p><r><t>Hello &amp; world</t></r></p><p><r><t>Second</t></r></p></body></document>`})
	doc, err := Extract(context.Background(), "report.docx", data)
	if err != nil || !strings.Contains(doc.Text, "Hello & world\nSecond\n") {
		t.Fatalf("%+v %v", doc, err)
	}
	for _, bad := range [][]byte{[]byte("not zip"), office(t, map[string]string{"word/document.xml": `<!DOCTYPE d [<!ENTITY x SYSTEM "file:///secret">]><d><t>&x;</t></d>`}), office(t, map[string]string{"word/document.xml": strings.Repeat("x", MaxText+1)})} {
		if _, err := Extract(context.Background(), "bad.docx", bad); err == nil {
			t.Fatal("accepted malformed, external entity or oversized archive")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Extract(ctx, "report.docx", data); err == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestDocumentXLSXValuesSparseCellsAndAnalysis(t *testing.T) {
	files := map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="urn:relationships"><sheets><sheet name="Sales" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><t>Amount</t></si><si><r><t>Re</t></r><r><t>gion</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row><row><c r="A2"><f>SUM(2,3)</f><v>5</v></c><c r="C2" t="inlineStr"><is><t>East</t></is></c></row><row><c r="A3"><v>15</v></c></row></sheetData></worksheet>`,
	}
	doc, err := Extract(context.Background(), "sales.xlsx", office(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Tables) != 1 || doc.Tables[0].Name != "Sales" || len(doc.Tables[0].Rows[1]) != 3 || doc.Tables[0].Rows[1][2] != "East" || !strings.Contains(doc.Text, "cached values") {
		t.Fatalf("%+v", doc)
	}
	stats, err := Analyze(doc, true)
	if err != nil || *stats[0].Columns[0].Mean != 10 || *stats[0].Columns[0].Sum != 20 || stats[0].Columns[1].Missing != 2 {
		t.Fatalf("%+v %v", stats, err)
	}
	files["xl/worksheets/sheet1.xml"] = `<worksheet><sheetData><row><c r="ZZZZZZZZ999"><v>1</v></c></row></sheetData></worksheet>`
	if _, err = Extract(context.Background(), "bad.xlsx", office(t, files)); err == nil {
		t.Fatal("accepted unbounded sparse cell")
	}
	files["xl/_rels/workbook.xml.rels"] = `<Relationships><Relationship Id="rId1" Target="https://example.com/x" TargetMode="External"/></Relationships>`
	if _, err = Extract(context.Background(), "bad.xlsx", office(t, files)); err == nil {
		t.Fatal("accepted external worksheet")
	}
}
func TestDocumentCSVAnalysisAndNonfinite(t *testing.T) {
	doc, err := Extract(context.Background(), "x.csv", []byte("name,value\na,1\nb,3\nc,\nd,NaN\n"))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Analyze(doc, true)
	if err != nil || stats[0].Rows != 4 || stats[0].Columns[1].Numeric != 2 || *stats[0].Columns[1].Mean != 2 || stats[0].Columns[1].Missing != 1 {
		t.Fatalf("%+v %v", stats, err)
	}
	if _, err = Extract(context.Background(), "x.csv", []byte("a,\"bad")); err == nil {
		t.Fatal("accepted broken CSV")
	}
}

func TestDocumentRepeatedSharedStringOutputRemainsBounded(t *testing.T) {
	var row strings.Builder
	for i := 0; i < 100; i++ {
		row.WriteString(`<c t="s"><v>0</v></c>`)
	}
	files := map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="urn:relationships"><sheets><sheet name="Large" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><t>` + strings.Repeat("x", 100000) + `</t></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row>` + row.String() + `</row></sheetData></worksheet>`,
	}
	if _, err := Extract(context.Background(), "large.xlsx", office(t, files)); err == nil {
		t.Fatal("shared-string expansion escaped output cap")
	}
}
func TestDocumentPDFMissingConverterAndInvalidHeader(t *testing.T) {
	t.Setenv("DEERFLOW_PDFTOTEXT", t.TempDir()+"/missing-pdftotext")
	if _, err := Extract(context.Background(), "x.pdf", []byte("%PDF-1.7\n")); err == nil || !strings.Contains(err.Error(), "Poppler") {
		t.Fatal(err)
	}
	if _, err := Extract(context.Background(), "x.pdf", []byte("not PDF")); err == nil {
		t.Fatal("accepted non-PDF")
	}
}
func TestDocumentPDFWithInstalledPoppler(t *testing.T) {
	exe, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("Poppler is not installed; optional live parser test")
	}
	t.Setenv("DEERFLOW_PDFTOTEXT", exe)
	// Minimal text-bearing fixture; Poppler reconstructs its missing xref table.
	pdf := []byte("%PDF-1.4\n1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj\n3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >> endobj\n4 0 obj << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> endobj\n5 0 obj << /Length 45 >> stream\nBT /F1 12 Tf 50 250 Td (Hello PDF) Tj ET\nendstream endobj\ntrailer << /Root 1 0 R >>\n%%EOF")
	doc, err := Extract(context.Background(), "x.pdf", pdf)
	if err != nil || !strings.Contains(doc.Text, "Hello PDF") {
		t.Fatalf("%s %v (%s)", doc.Text, err, os.Getenv("DEERFLOW_PDFTOTEXT"))
	}
}
