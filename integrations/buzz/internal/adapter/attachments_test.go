package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func attachmentFixture() ([]byte, Attachment, [][]string) {
	data := []byte("\x89PNG\r\n\x1a\nfixture")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	a := Attachment{URL: "https://relay.invalid/media/" + hash + ".png", Name: "image.png", MIME: "image/png", SHA256: hash, Size: int64(len(data))}
	return data, a, [][]string{{"imeta", "url " + a.URL, "m " + a.MIME, "x " + hash, fmt.Sprintf("size %d", a.Size), "filename image.png"}}
}
func TestAttachmentMetadataGuards(t *testing.T) {
	_, _, tags := attachmentFixture()
	for _, tc := range []struct {
		name, value string
		index       int
	}{
		{"foreign origin", "url https://evil.invalid/media/" + strings.Repeat("a", 64), 1},
		{"wrong hash path", "url https://relay.invalid/media/" + strings.Repeat("a", 64), 1},
		{"credentials", "url https://user:password@relay.invalid/media/" + strings.Repeat("a", 64), 1},
		{"invalid hash", "x wrong", 3}, {"oversize", "size 999999999", 4}, {"audio", "m audio/mpeg", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyTag := append([]string{}, tags[0]...)
			copyTag[tc.index] = tc.value
			if _, e := ParseAttachments([][]string{copyTag}, "wss://relay.invalid"); e == nil {
				t.Fatal("invalid attachment accepted")
			}
		})
	}
	if a, e := ParseAttachments(tags, "wss://relay.invalid"); e != nil || len(a) != 1 {
		t.Fatal(a, e)
	}
}
func TestAttachmentVerificationAndCache(t *testing.T) {
	data, a, _ := attachmentFixture()
	workspace := t.TempDir()
	downloads := 0
	b := BuzzCLI{Config: testConfig(t), runner: func(_ context.Context, _ []string, _ string, _ bool, out io.Writer, _ int64) ([]byte, error) {
		downloads++
		_, e := out.Write(data)
		return nil, e
	}}
	for i := 0; i < 2; i++ {
		blocks, e := PrepareAttachments(context.Background(), &b, workspace, "s", "e", []Attachment{a})
		if e != nil || len(blocks) != 1 || blocks[0]["type"] != "image" {
			t.Fatal(blocks, e)
		}
	}
	if downloads != 1 {
		t.Fatal("cache not reused")
	}
	bad := a
	bad.SHA256 = strings.Repeat("a", 64)
	if _, e := PrepareAttachments(context.Background(), &b, workspace, "s", "bad", []Attachment{bad}); e == nil {
		t.Fatal("tampering accepted")
	}
	bad = a
	bad.MIME = "image/jpeg"
	if _, e := PrepareAttachments(context.Background(), &b, workspace, "s", "badmime", []Attachment{bad}); e == nil {
		t.Fatal("MIME tampering accepted")
	}
}
func TestAttachmentCacheRejectsLinks(t *testing.T) {
	workspace := t.TempDir()
	target := t.TempDir()
	if e := os.Symlink(target, filepath.Join(workspace, ".buzz-attachments")); e != nil {
		t.Skip("symlinks unavailable")
	}
	if root, _, e := openCache(workspace, "s", "e"); e == nil {
		root.Close()
		t.Fatal("cache followed symlink")
	}
}
func TestAttachmentOrdinaryFileResource(t *testing.T) {
	data := []byte("file")
	sum := sha256.Sum256(data)
	a := Attachment{Name: "report.txt", MIME: "text/plain", SHA256: hex.EncodeToString(sum[:]), Size: 4}
	b := BuzzCLI{Config: testConfig(t), runner: func(_ context.Context, _ []string, _ string, _ bool, out io.Writer, _ int64) ([]byte, error) {
		_, e := io.Copy(out, bytes.NewReader(data))
		return nil, e
	}}
	blocks, e := PrepareAttachments(context.Background(), &b, t.TempDir(), "s", "e", []Attachment{a})
	if e != nil || len(blocks) != 1 || blocks[0]["type"] != "resource_link" || !strings.HasPrefix(blocks[0]["uri"].(string), "file:///") {
		t.Fatal(blocks, e)
	}
}
