package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func versionTools(t *testing.T, cwd string, migrated bool) map[string]tool.InvokableTool {
	t.Helper()
	var configs []harness.BuiltinToolConfig
	if migrated {
		configs = []harness.BuiltinToolConfig{{Name: "write_file"}, {Name: "read_file"}, {Name: "str_replace"}}
	}
	items, cleanup, err := ConfiguredWorkspaceFactory(context.Background(), harness.RunRequest{Session: harness.Session{CWD: cwd}}, configs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	named := map[string]tool.InvokableTool{}
	for _, item := range items {
		info, _ := item.Info(context.Background())
		named[info.Name] = item.(tool.InvokableTool)
	}
	return named
}

func versionCall(t *testing.T, tools map[string]tool.InvokableTool, name string, args any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tools[name].InvokableRun(context.Background(), string(b))
}

func TestVersionedWorkspaceConflictsAndFreshRead(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "configured"}[migrated], func(t *testing.T) {
			cwd := testutil.CanonicalTempDir(t)
			tools := versionTools(t, cwd, migrated)
			out, err := versionCall(t, tools, "write_file", map[string]any{"path": "note", "content": "first"})
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err = json.Unmarshal([]byte(out), &result); err != nil || result["version"] != fileVersion("first") {
				t.Fatalf("%s %v", out, err)
			}
			for _, version := range []string{"", "absent", "stale"} {
				_, err = versionCall(t, tools, "write_file", map[string]any{"path": "note", "content": "lost", "expected_version": version})
				var notExecuted *harness.ToolNotExecutedError
				if !errors.As(err, &notExecuted) {
					t.Fatalf("unprotected overwrite %q: %v", version, err)
				}
			}
			out, err = versionCall(t, tools, "read_file", map[string]any{"path": "note"})
			if err != nil || json.Unmarshal([]byte(out), &result) != nil {
				t.Fatal(out, err)
			}
			// An external edit while an approval is outstanding invalidates it.
			if err = os.WriteFile(filepath.Join(cwd, "note"), []byte("external"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = versionCall(t, tools, "write_file", map[string]any{"path": "note", "content": "lost", "expected_version": result["version"]})
			if err == nil {
				t.Fatal("accepted stale approved version")
			}
			out, err = versionCall(t, tools, "write_file", map[string]any{"path": "note", "content": "fresh", "expected_version": fileVersion("external")})
			if err != nil {
				t.Fatal(out, err)
			}
			edit, args := "edit_file", map[string]any{"path": "note", "old_text": "fresh", "new_text": "edited", "expected_version": fileVersion("fresh")}
			if migrated {
				edit = "str_replace"
				args = map[string]any{"path": "note", "old_str": "fresh", "new_str": "edited", "expected_version": fileVersion("fresh")}
			}
			if _, err = versionCall(t, tools, edit, args); err != nil {
				t.Fatal(err)
			}
			if _, err = versionCall(t, tools, edit, args); err == nil {
				t.Fatal("stale replacement accepted")
			}
			b, _ := os.ReadFile(filepath.Join(cwd, "note"))
			if string(b) != "edited" {
				t.Fatal(string(b))
			}
		})
	}
}

func TestVersionedWorkspaceConcurrentFactories(t *testing.T) {
	cwd := testutil.CanonicalTempDir(t)
	if err := os.WriteFile(filepath.Join(cwd, "note"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	one, two := versionTools(t, cwd, false), versionTools(t, cwd, true)
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	var wg sync.WaitGroup
	for _, tools := range []map[string]tool.InvokableTool{one, two} {
		wg.Add(1)
		go func(tools map[string]tool.InvokableTool) {
			defer wg.Done()
			<-start
			args, _ := json.Marshal(map[string]any{"path": "note", "content": "changed", "expected_version": fileVersion("base")})
			_, err := tools["write_file"].InvokableRun(context.Background(), string(args))
			outcomes <- err
		}(tools)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	success := 0
	for err := range outcomes {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful writers=%d", success)
	}
}
