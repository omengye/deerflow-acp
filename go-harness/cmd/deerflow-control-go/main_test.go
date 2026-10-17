package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDocumentPreviewIsBoundedAndDoesNotContactDaemon(t *testing.T) {
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	f, _ := z.Create("word/document.xml")
	_, _ = f.Write([]byte("<document><p><t>" + strings.Repeat("text ", 5000) + "</t></p></document>"))
	_ = z.Close()
	path := filepath.Join(t.TempDir(), "report.docx")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"--preview-file", path}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil || len(got.Text) != 8192 || !got.Truncated {
		t.Fatal(len(got.Text), got.Truncated, err)
	}
	if err := run([]string{"--preview-file", path, "--request", "request.json"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("mixed management and local preview accepted")
	}
}
