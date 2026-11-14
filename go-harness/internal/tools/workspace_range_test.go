package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileLineRangesCoverLargeFiles(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; b.Len() <= maxFileBytes; i++ {
		fmt.Fprintf(&b, "line %06d %s\n", i, strings.Repeat("x", 40))
	}
	content := b.String()
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := readVersionedFile(context.Background(), root, "big.txt"); err == nil || !strings.Contains(err.Error(), "offset and limit") {
		t.Fatalf("whole-file read should point to ranged reads: %v", err)
	}
	raw, err := readVersionedFileRange(context.Background(), root, "big.txt", 5000, 3)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version            string
		StartLine, EndLine int
		TotalLines         int
		Content            string
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.StartLine != 5000 || got.EndLine != 5002 || !strings.HasPrefix(got.Content, "line 005000 ") || strings.Count(got.Content, "\n") != 3 || got.Version != fileVersion(content) || got.TotalLines != strings.Count(content, "\n") {
		t.Fatalf("unexpected range: %+v", got)
	}
	raw, err = readVersionedFileRange(context.Background(), root, "big.txt", got.TotalLines+10, 0)
	if err != nil || !strings.Contains(raw, `"content":""`) {
		t.Fatalf("past-end read: %s %v", raw, err)
	}
	if _, err := readVersionedFileRange(context.Background(), root, "big.txt", -1, 0); err == nil {
		t.Fatal("negative offset accepted")
	}
}
