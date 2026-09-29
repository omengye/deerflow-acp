package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestWorkspaceIOAndTraversal(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	if err := writeFile(ctx, cwd, "nested/note.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(ctx, cwd, "nested/note.txt", "updated"); err != nil {
		t.Fatal(err)
	}
	text, err := readFile(ctx, cwd, "nested/note.txt")
	if err != nil || text != "updated" {
		t.Fatalf("%q %v", text, err)
	}
	for _, path := range []string{"../escape.txt", filepath.Join(t.TempDir(), "escape.txt")} {
		if err := writeFile(ctx, cwd, path, "escape"); err == nil {
			t.Fatalf("accepted %s", path)
		}
		if _, err := readFile(ctx, cwd, path); err == nil {
			t.Fatalf("read accepted %s", path)
		}
	}
	if _, err := readFile(ctx, cwd, "."); err == nil {
		t.Fatal("read accepted a directory")
	}
}

func TestRunKeepsPinnedWorkspaceDirectory(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "note"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	items, cleanup, err := WorkspaceFactory(context.Background(), harness.RunRequest{Session: harness.Session{CWD: cwd}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err = os.Rename(cwd, filepath.Join(base, "moved")); err != nil {
		t.Logf("OS prevents replacing the pinned root: %v", err)
		return
	}
	if err = os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(cwd, "note"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		info, _ := item.Info(context.Background())
		if info.Name == "read_file" {
			value, err := item.(tool.InvokableTool).InvokableRun(context.Background(), `{"path":"note"}`)
			if err != nil || !strings.Contains(value, "original") || strings.Contains(value, "replacement") {
				t.Fatalf("root was reopened: %q %v", value, err)
			}
			return
		}
	}
	t.Fatal("read tool missing")
}

func TestRejectRedirectedWorkspaceRoot(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "saved-path")
	outside := t.TempDir()
	if err := os.Symlink(outside, cwd); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, cleanup, err := WorkspaceFactory(context.Background(), harness.RunRequest{Session: harness.Session{CWD: cwd}})
	if cleanup != nil {
		_ = cleanup()
	}
	if err == nil {
		t.Fatal("accepted a redirected saved workspace")
	}
}
func TestWorkspaceSymlinkEscape(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readFile(context.Background(), cwd, "link/secret"); err == nil {
		t.Fatal("symlink read escaped")
	}
	if err := writeFile(context.Background(), cwd, "link/created", "bad"); err == nil {
		t.Fatal("symlink write escaped")
	}
}
func TestPlanModeOmitsWriteTools(t *testing.T) {
	tools, cleanup, err := WorkspaceFactory(context.Background(), harness.RunRequest{Session: harness.Session{CWD: t.TempDir(), Mode: "plan"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, tool := range tools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "write_file" || info.Name == "edit_file" {
			t.Fatal("plan mode can write")
		}
	}
}
