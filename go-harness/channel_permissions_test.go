package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestChannelGlobalPolicyNativeToolsAndDesktopResume(t *testing.T) {
	for _, mode := range []harness.PermissionMode{harness.PermissionModeOff, harness.PermissionModeDangerous, harness.PermissionModeAll} {
		for _, existing := range []bool{false, true} {
			name := string(mode) + "/new"
			if existing {
				name = string(mode) + "/existing-ask"
			}
			t.Run(name, func(t *testing.T) {
				workspace, cfg := skillFixture(t)
				cfg.PermissionMode = mode
				cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
				calls := []struct{ name, args, evidence string }{
					{"skill", `{"skill":"research"}`, "BODY_ONLY_AFTER_LOAD"},
					{"skill_inspect", `{"name":"research"}`, "research"},
					{"list_directory", `{"path":"."}`, "skills"},
					{"write_file", `{"path":"approved.txt","content":"approved fixture"}`, ""},
				}
				var modelCalls atomic.Int64
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					modelCalls.Add(1)
					var req struct {
						Stream   bool
						Messages []struct{ Role, Content string }
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						http.Error(w, "fixture request", 400)
						return
					}
					n := 0
					for _, m := range req.Messages {
						if m.Role != "tool" {
							continue
						}
						if strings.Contains(m.Content, "permission denied") || (n < len(calls) && !strings.Contains(m.Content, calls[n].evidence)) {
							t.Errorf("unexpected tool result %d: %s", n, m.Content)
						}
						n++
					}
					if n < len(calls) {
						skillToolResponse(w, req.Stream, calls[n].name, calls[n].args, "")
					} else {
						skillToolResponse(w, req.Stream, "", "", "verified result")
					}
				}))
				defer provider.Close()
				cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				c, err := Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				sessionID := ""
				if existing {
					x, err := c.service.NewSession(ctx, "previous-owner", workspace)
					if err != nil {
						t.Fatal(err)
					}
					if x.ApprovalMode != harness.ApprovalAsk {
						t.Fatal("fixture did not start with old channel approval")
					}
					sessionID = x.ID
					if err = c.service.Disconnect(ctx, "previous-owner"); err != nil {
						t.Fatal(err)
					}
				}
				out, err := c.channelRun(ctx, sessionID, workspace, "Load research, inspect it, list the directory, then write the fixture file.", harness.ApprovalGlobal, func(id string) error {
					if sessionID != "" && sessionID != id {
						t.Error("existing channel session was replaced")
					}
					sessionID = id
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.Text, "工具调用摘要") || !strings.Contains(out.Text, "skill") || strings.Contains(out.Text+out.Attention, "approved fixture") {
					t.Fatalf("missing or unsafe tool feedback: %+v", out)
				}
				if mode != harness.PermissionModeOff {
					before, err := c.service.Store.Execution(ctx, sessionID, "")
					if err != nil {
						t.Fatal(err)
					}
					count := modelCalls.Load()
					followup, err := c.channelRun(ctx, sessionID, workspace, "hi", harness.ApprovalGlobal, func(string) error { return nil })
					if err != nil || followup.Status != "needs_attention" || !strings.Contains(followup.Attention, before.WaitingInputs[0].ToolName) || !strings.Contains(followup.Attention, "/new") || modelCalls.Load() != count {
						t.Fatalf("waiting follow-up lost feedback or reran: %+v %v", followup, err)
					}
					fresh, err := c.channelRun(ctx, sessionID, workspace, "/new", harness.ApprovalGlobal, func(id string) error {
						if id == sessionID {
							t.Error("/new reused waiting session")
						}
						return nil
					})
					if err != nil || fresh.Status != "completed" || modelCalls.Load() != count {
						t.Fatalf("/new invoked model or failed: %+v %v", fresh, err)
					}
					after, err := c.service.Store.Execution(ctx, sessionID, "")
					if err != nil || before.Version != after.Version || before.Status != after.Status {
						t.Fatal("/new or follow-up changed the waiting execution", err)
					}
				}
				// A desktop owner must be able to attach after the channel releases it.
				x, err := c.LoadSession(ctx, sessionID, workspace, false, nil)
				if err != nil || x.ApprovalMode != harness.ApprovalGlobal {
					t.Fatalf("session=%+v err=%v", x, err)
				}
				state, err := c.Execution(ctx, sessionID, "")
				if err != nil {
					t.Fatal(err)
				}
				if mode == harness.PermissionModeOff {
					if out.Status != "completed" || state.Status != harness.ExecutionCompleted {
						t.Fatalf("global off blocked: %+v %+v", out, state)
					}
				} else {
					if out.Status != "needs_attention" || state.Status != harness.ExecutionWaitingInput || !state.Resumable {
						t.Fatalf("approval was not preserved: %+v %+v", out, state)
					}
					if _, err = os.Stat(filepath.Join(workspace, "approved.txt")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("write executed before desktop approval", err)
					}
					var completed, grants int
					if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_tool_receipts WHERE session_id=? AND state='completed'`, sessionID).Scan(&completed); err != nil {
						t.Fatal(err)
					}
					want := 0
					if mode == harness.PermissionModeDangerous {
						want = 3
					}
					if completed != want {
						t.Fatalf("completed reads=%d want=%d", completed, want)
					}
					if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_execution_grants WHERE run_id=?`, state.RunID).Scan(&grants); err != nil || grants != 0 {
						t.Fatal("channel manufactured an approval or denial", grants, err)
					}
					approvals := 0
					resumed, err := c.ResumeExecution(ctx, sessionID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
						approvals++
						if mode == harness.PermissionModeDangerous && p.ToolName != "write_file" {
							t.Errorf("read unexpectedly required approval: %s", p.ToolName)
						}
						return harness.AllowOnce, nil
					})
					wantApprovals := 1
					if mode == harness.PermissionModeAll {
						wantApprovals = len(calls)
					}
					if err != nil || resumed.Execution == nil || resumed.Execution.Status != harness.ExecutionCompleted || approvals != wantApprovals {
						t.Fatalf("resume=%+v approvals=%d want=%d err=%v", resumed, approvals, wantApprovals, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(workspace, "approved.txt"))
				if err != nil || string(data) != "approved fixture" {
					t.Fatalf("write result=%q err=%v", data, err)
				}
			})
		}
	}
}

func TestChannelWaitingWebFetchFeedbackSurvivesRestart(t *testing.T) {
	workspace, cfg := skillFixture(t)
	cfg.PermissionMode = harness.PermissionModeDangerous
	cfg.BuiltinTools = []harness.BuiltinToolConfig{{Name: "web_fetch"}}
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		skillToolResponse(w, req.Stream, "web_fetch", `{"url":"https://example.invalid/health?token=DO-NOT-FORWARD"}`, "")
	}))
	defer provider.Close()
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
	ctx := context.Background()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	first, err := c.channelRun(ctx, "", workspace, "check health", harness.ApprovalGlobal, func(value string) error { id = value; return nil })
	if err != nil || first.Status != "needs_attention" || !strings.Contains(first.Attention, "web_fetch") || strings.Contains(first.Text+first.Attention, "DO-NOT-FORWARD") {
		c.Close()
		t.Fatal(first, err)
	}
	before, err := c.service.Store.Execution(ctx, id, "")
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	followup, err := c.channelRun(ctx, id, workspace, "hi", harness.ApprovalGlobal, func(value string) error {
		if value != id {
			t.Error("waiting session replaced")
		}
		return nil
	})
	if err != nil || followup.Status != "needs_attention" || !strings.Contains(followup.Attention, "web_fetch") || strings.Contains(followup.Attention, "DO-NOT-FORWARD") || calls.Load() != 1 {
		t.Fatal(followup, err, calls.Load())
	}
	after, err := c.service.Store.Execution(ctx, id, "")
	if err != nil || after.Version != before.Version || after.Status != harness.ExecutionWaitingInput {
		t.Fatal(after, err)
	}
}
