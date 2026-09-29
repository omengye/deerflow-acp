package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func skillFixture(t *testing.T) (string, Config) {
	t.Helper()
	workspace := t.TempDir()
	root := filepath.Join(workspace, "skills")
	pkg := filepath.Join(root, "research")
	if err := os.MkdirAll(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "SKILL.md"), []byte("---\nname: research\ndescription: Research a source\n---\nBODY_ONLY_AFTER_LOAD. Read guide.txt.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "guide.txt"), []byte("PINNED_REFERENCE"), 0600); err != nil {
		t.Fatal(err)
	}
	return workspace, Config{DataDir: t.TempDir(), Skills: harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: workspace}}}, DisableSubagents: true}
}

func TestSDKSkillsUseNativeProgressiveLoadingAndPersistentInstall(t *testing.T) {
	workspace, cfg := skillFixture(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "read", 500)
			return
		}
		call := calls.Add(1)
		body := string(data)
		var name, args string
		switch call {
		case 1:
			if !strings.Contains(body, "Research a source") || strings.Contains(body, "BODY_ONLY_AFTER_LOAD") || strings.Contains(body, "PINNED_REFERENCE") {
				t.Errorf("first model request did not use metadata-only projection")
			}
			name, args = "skill", `{"skill":"research"}`
		case 2:
			if !strings.Contains(body, "BODY_ONLY_AFTER_LOAD") || strings.Contains(body, "PINNED_REFERENCE") {
				t.Errorf("skill body/ref progressive projection is wrong")
			}
			name, args = "read_skill_file", `{"skill":"research","path":"guide.txt"}`
		case 3:
			if !strings.Contains(body, "PINNED_REFERENCE") {
				t.Errorf("reference file absent from model history")
			}
		default:
			t.Errorf("unexpected model request %d", call)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"role": "assistant", "content": "done"}
		reason := "stop"
		if name != "" {
			delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("skill-%d", call), "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}
			reason = "tool_calls"
		}
		chunk, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer provider.Close()
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	record, err := c.InstallSkill(context.Background(), "project", "research")
	if err != nil || record.Enabled {
		t.Fatalf("install=%+v err=%v", record, err)
	}
	if err = c.SetSkillEnabled(context.Background(), "project", "research", true); err != nil {
		t.Fatal(err)
	}
	session, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetConfigOption(context.Background(), session.ID, "approval", harness.ApprovalReadOnly); err != nil {
		t.Fatal(err)
	}
	_, err = c.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "Use research"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		t.Error("read-only skill reference unexpectedly requested approval")
		return harness.RejectOnce, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
	receipts, err := c.ListToolReceipts(context.Background(), session.ID)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	for _, receipt := range receipts {
		if receipt.State != harness.ReceiptCompleted {
			t.Fatalf("receipt=%+v", receipt)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	records, err := c.ListSkills(context.Background(), harness.SkillSelection{Workspace: workspace})
	if err != nil || len(records) != 1 || !records[0].Enabled || records[0].Ref != record.Ref {
		t.Fatalf("reopened skills=%+v err=%v", records, err)
	}
}

func TestExtensionFactoryRestoresPinnedSkillAndRejectsChangedResources(t *testing.T) {
	workspace, cfg := skillFixture(t)
	db, err := sqlite.Open(filepath.Join(cfg.DataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := skills.NewRegistry(context.Background(), cfg.Skills, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if _, err = registry.Install(context.Background(), "project", "research"); err != nil {
		t.Fatal(err)
	}
	if err = registry.SetEnabled(context.Background(), "project", "research", true); err != nil {
		t.Fatal(err)
	}
	manager, err := mcp.New(harness.MCPPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	req := harness.RunRequest{Session: harness.Session{ID: "s", CWD: workspace, Mode: "plan"}}
	factory := extensionFactory(cfg, manager, registry)
	first, err := factory(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cleanup()
	if err = os.WriteFile(filepath.Join(cfg.Skills.Sources[0].Root, "research", "guide.txt"), []byte("NEW_REFERENCE"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Install(context.Background(), "project", "research"); err != nil {
		t.Fatal(err)
	}
	if err = registry.SetEnabled(context.Background(), "project", "research", false); err != nil {
		t.Fatal(err)
	}
	restored, err := factory(context.Background(), req, first.State)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Cleanup()
	if string(first.State) != string(restored.State) {
		t.Fatal("checkpoint skills changed")
	}
	read := func(ext einoengine.RunExtensions) string {
		for _, x := range ext.Tools {
			info, _ := x.Info(context.Background())
			if info.Name == "read_skill_file" {
				out, err := x.(tool.InvokableTool).InvokableRun(context.Background(), `{"skill":"research","path":"guide.txt"}`)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
		}
		t.Fatal("skill reference tool missing")
		return ""
	}
	if read(restored) != "PINNED_REFERENCE" {
		t.Fatal("resume did not restore exact installed version")
	}
	next, err := factory(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Cleanup()
	if len(next.Handlers) != 0 {
		t.Fatal("disabled skill appears in new run")
	}
	req.Session.CWD = t.TempDir()
	if _, err = factory(context.Background(), req, first.State); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("cross-workspace resume=%v", err)
	}
	req.Session.CWD = workspace
	cfg.Sandbox.AllowShell = true
	if _, err = extensionFactory(cfg, manager, registry)(context.Background(), req, first.State); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("changed policy resume=%v", err)
	}
}
