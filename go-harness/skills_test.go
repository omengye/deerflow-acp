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
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func skillFixture(t *testing.T) (string, Config) {
	t.Helper()
	workspace := testutil.CanonicalTempDir(t)
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

func TestSkillsExplicitStartupProfileIsAppliedAndIdempotent(t *testing.T) {
	workspace, cfg := skillFixture(t)
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	var ref harness.SkillRef
	for i := 0; i < 2; i++ {
		c, err := Open(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		records, err := c.ListSkills(context.Background(), harness.SkillSelection{Workspace: workspace})
		if err != nil || len(records) != 1 || !records[0].Enabled {
			_ = c.Close()
			t.Fatalf("profile=%+v %v", records, err)
		}
		if i == 0 {
			ref = records[0].Ref
		} else if records[0].Ref != ref {
			t.Fatal("unchanged profile created different version")
		}
		if err = c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Skills.Install[0].Enabled = false
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	records, err := c.ListSkills(context.Background(), harness.SkillSelection{Workspace: workspace})
	if err != nil || len(records) != 1 || records[0].Enabled {
		t.Fatalf("explicit disabled profile=%+v %v", records, err)
	}
}

func TestSkillsPublishedProfileSurvivesSDKReopenAndDeletion(t *testing.T) {
	workspace, cfg := skillFixture(t)
	ctx := context.Background()
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	selection := harness.SkillSelection{Workspace: workspace}
	original, err := c.skills.Snapshot(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := c.skills.PrepareCandidate("research", map[string][]byte{"SKILL.md": []byte("---\nname: research\ndescription: Reviewed research\n---\nReviewed new instructions.\n")})
	if err != nil {
		t.Fatal(err)
	}
	err = c.skills.WithPublication(ctx, "project", "research", func(tx *skills.PublicationTx) error {
		head, err := tx.Current()
		if err != nil {
			return err
		}
		v, err := tx.Stage(candidate)
		if err != nil {
			return err
		}
		_, err = tx.Publish(v, head, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := c.skills.Snapshot(ctx, selection)
	if err != nil || len(fresh.Refs()) != 1 || fresh.Refs()[0].Version != candidate.Version() {
		t.Fatal("restart did not use published files", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	sourceFile := filepath.Join(cfg.Skills.Sources[0].Root, "research", "SKILL.md")
	if err = os.Remove(sourceFile); err != nil {
		t.Fatal(err)
	}
	cfg.Skills.Install = nil // Missing bindings must be rechecked even without discovery.
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.QuerySkillCatalog(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Skills) != 1 || page.Skills[0].SourceStatus != "unavailable" {
		t.Fatal("missing directory advertised as ready", page, err)
	}
	fresh, err = c.skills.Snapshot(ctx, selection)
	if err != nil || len(fresh.Refs()) != 0 {
		t.Fatal("missing files used cached release", err)
	}
	restored, err := c.skills.Restore(ctx, selection, original.Refs())
	if err != nil {
		t.Fatal(err)
	}
	if old, err := restored.ReadFile(ctx, "research", "guide.txt"); err != nil || string(old) != "PINNED_REFERENCE" {
		t.Fatal("explicit historical checkpoint lost support file", err)
	}
	if err = os.WriteFile(sourceFile, candidate.Files()["SKILL.md"], 0600); err != nil {
		t.Fatal(err)
	}
	if err = c.RefreshSkillImports(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteSkill(ctx, "project", "research"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(sourceFile); !os.IsNotExist(err) {
		t.Fatal("delete did not remove authoritative files", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := c.ListSkills(ctx, selection); err != nil || len(records) != 0 {
		t.Fatal("deleted source resurrected", err)
	}
	if err = os.MkdirAll(filepath.Dir(sourceFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(sourceFile, candidate.Files()["SKILL.md"], 0600); err != nil {
		t.Fatal(err)
	}
	if err = c.RefreshSkillImports(ctx); err != nil {
		t.Fatal(err)
	}
	if head, err := c.GetSkillHead(ctx, "project", "research"); err != nil || !head.Exists || head.Enabled {
		t.Fatal("restored files not indexed with durable disabled state", head, err)
	}
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
	result, err := c.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "Use research"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		t.Error("read-only skill reference unexpectedly requested approval")
		return harness.RejectOnce, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
	assertEvidence := func(c *Client) {
		t.Helper()
		var loads, support, total, outcomes int
		err := c.store.DB().QueryRow(`SELECT
 (SELECT count(*) FROM harness_skill_reads WHERE kind='skill_load'),
 (SELECT count(*) FROM harness_skill_reads WHERE kind='support_read'),
 (SELECT count(*) FROM harness_skill_reads WHERE session_id=? AND run_id=? AND root_id=? AND role_id='' AND version=?),
 (SELECT count(*) FROM harness_skill_outcomes WHERE run_id=? AND status='completed' AND effects_uncertain=0)`,
			session.ID, result.Execution.RunID, result.Execution.RunID, record.Ref.Version, result.Execution.RunID).Scan(&loads, &support, &total, &outcomes)
		if err != nil || loads != 1 || support != 1 || total != 2 || outcomes != 1 {
			t.Fatalf("persistent SDK evidence loads=%d support=%d matching=%d outcomes=%d err=%v", loads, support, total, outcomes, err)
		}
	}
	assertEvidence(c)
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
	assertEvidence(c)
	signal := waitSkillSignal(t, context.Background(), c, 2)
	if signal.RootID != result.Execution.RunID || signal.CompletedRuns != 1 || signal.Version != record.Ref.Version || calls.Load() != 3 {
		t.Fatalf("collector restart changed execution or invoked models: %+v calls=%d", signal, calls.Load())
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
	factory := extensionFactory(cfg, manager, registry, nil)
	first, err := factory(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cleanup()
	filtered, err := extensionFactory(Config{Skills: cfg.Skills, ToolPolicy: harness.ToolPolicy{Denylist: []string{"skill"}}}, manager, registry, nil)(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer filtered.Cleanup()
	if len(filtered.Handlers) != 0 {
		t.Fatal("host-denied skill middleware still injected its native tool")
	}
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
	if _, err = extensionFactory(cfg, manager, registry, nil)(context.Background(), req, first.State); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("changed policy resume=%v", err)
	}
	cfg.Sandbox.AllowShell = false
	cfg.ToolPolicy = harness.ToolPolicy{Allowlist: []string{"read_file"}}
	if _, err = extensionFactory(cfg, manager, registry, nil)(context.Background(), req, first.State); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("changed host tool allowlist resumed=%v", err)
	}
}
