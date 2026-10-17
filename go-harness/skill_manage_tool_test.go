package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func skillToolResponse(w http.ResponseWriter, stream bool, name, args, text string, callIDs ...string) {
	message := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if name != "" {
		callID := "call-" + name
		if len(callIDs) > 0 {
			callID = callIDs[0]
		}
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{"index": 0, "id": callID, "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
		finish = "tool_calls"
	}
	usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}, "usage": usage})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	} else {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
	}
}

func TestSkillManageNativeCandidateScopeAndBudget(t *testing.T) {
	for _, mode := range []string{"root", "role", "background-role", "managed-create", "builtin-overlay", "builtin-create", "hidden-role-skill", "quality-rejected", "quality-unavailable", "review-fails", "review-storage-fails", "budget-exhausted", "read-only", "tool-denied"} {
		t.Run(mode, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.SkillProposalSources = []string{"project"}
			cfg.PermissionMode = harness.PermissionModeOff
			if mode == "builtin-overlay" || mode == "builtin-create" {
				cfg.Skills.BuiltinSources = []string{"project"}
			}
			if mode == "managed-create" {
				cfg.Skills.Install = nil
				cfg.Skills.Sources[0].Root = filepath.Join(workspace, "generated")
				if err := os.MkdirAll(cfg.Skills.Sources[0].Root, 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Skills.ManagedSources = []string{"project"}
				cfg.Skills.ExportRoot = cfg.Skills.Sources[0].Root
			}
			limits := harness.BudgetLimits{MaxModelCalls: 8, MaxToolCalls: 6, MaxTokens: 100000, MaxOutputTokens: 2048}
			if mode == "budget-exhausted" {
				limits.MaxModelCalls = 2
			}
			cfg.Budget = &limits
			var mu sync.Mutex
			calls := map[string]int{}
			var toolResult string
			roleMode := mode == "role" || mode == "background-role" || mode == "hidden-role-skill"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model    string                                       `json:"model"`
					Stream   bool                                         `json:"stream"`
					Messages []struct{ Role, Content, ToolCallID string } `json:"messages"`
					Tools    []struct{ Function struct{ Name string } }   `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", 400)
					return
				}
				mu.Lock()
				calls[request.Model]++
				number := calls[request.Model]
				mu.Unlock()
				if request.Model == "moderator" || request.Model == "evaluator" {
					if mode == "quality-unavailable" && request.Model == "evaluator" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":{"message":"private-review-error","type":"unavailable"}}`)
						return
					}
					if mode == "review-fails" {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"message":"private-review-error"}}`)
						return
					}
					if len(request.Tools) != 0 || len(request.Messages) != 2 {
						t.Errorf("review borrowed tool/history context: %+v", request)
					}
					var input map[string]any
					if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
						t.Error(err)
					}
					passed, reason := true, "Scoped content review"
					if mode == "quality-rejected" && request.Model == "evaluator" {
						passed, reason = false, "Missing output consistency check."
					}
					decision, _ := json.Marshal(map[string]any{"base_version": input["base_version"], "candidate_version": input["candidate_version"], "passed": passed, "reason": reason})
					skillToolResponse(w, request.Stream, "", "", string(decision))
					return
				}
				for _, m := range request.Messages {
					if m.Role == "tool" && strings.Contains(m.Content, `"live_skill_changed"`) {
						mu.Lock()
						toolResult = m.Content
						mu.Unlock()
					}
				}
				if (mode == "quality-rejected" || mode == "quality-unavailable") && number == 2 {
					mu.Lock()
					priorResult := toolResult
					mu.Unlock()
					var prior struct {
						ProposalID string `json:"proposal_id"`
					}
					if err := json.Unmarshal([]byte(priorResult), &prior); err != nil || prior.ProposalID == "" {
						t.Errorf("missing proposal for get: %s %v", priorResult, err)
					}
					args, _ := json.Marshal(map[string]string{"action": "get", "proposal_id": prior.ProposalID})
					skillToolResponse(w, request.Stream, "skill_manage", string(args), "", "call-skill-manage-get")
					return
				}
				if number == 1 {
					visible := false
					for _, entry := range request.Tools {
						visible = visible || entry.Function.Name == "skill_manage"
					}
					want := mode != "read-only" && mode != "tool-denied"
					if visible != want {
						t.Errorf("skill_manage visible=%t, want=%t for %s", visible, want, request.Model)
					}
					if !want {
						skillToolResponse(w, request.Stream, "", "", "No candidate tool available")
						return
					}
					if roleMode && request.Model == "parent" {
						if mode == "background-role" {
							skillToolResponse(w, request.Stream, "background_agent", `{"role":"editor","instruction":"Propose the authorized skill change"}`, "")
							return
						}
						skillToolResponse(w, request.Stream, "task", `{"subagent_type":"editor","prompt":"Propose the authorized skill change","description":"Improve research"}`, "")
						return
					}
					args := `{"action":"patch","source":"project","name":"research","reason":"Make research steps clearer","find":"BODY_ONLY_AFTER_LOAD","replace":"IMPROVED_RESEARCH_STEPS","expected_count":1}`
					if mode == "builtin-create" {
						args = `{"action":"create","source":"project","name":"unregistered","reason":"New reusable steps","content":"---\nname: unregistered\ndescription: New skill\n---\nFollow the steps."}`
					}
					if mode == "managed-create" {
						data, _ := json.Marshal(map[string]any{"action": "create", "source": "project", "name": "research", "reason": "New reusable steps", "files": map[string]string{"SKILL.md": "---\nname: research\ndescription: New skill\n---\nFollow the reviewed steps.", "assets/example.json": "{\"format\":\"markdown\"}"}})
						args = string(data)
					}
					skillToolResponse(w, request.Stream, "skill_manage", args, "")
					return
				}
				skillToolResponse(w, request.Stream, "", "", "Candidate processing finished")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "parent", server.URL+"/v1", "fixture"
			reviewRoute := harness.ModelRoute{Provider: "openai", Model: "moderator", BaseURL: server.URL + "/v1", APIKey: "fixture"}
			quality := reviewRoute
			quality.Model = "evaluator"
			var err error
			cfg.SkillValidator, err = NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: reviewRoute, Evaluation: quality, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			if roleMode {
				cfg.DisableSubagents = false
				child := reviewRoute
				child.Model = "child"
				cfg.ModelRoutes = map[string]harness.ModelRoute{"child": child}
				selected := []string{"research"}
				if mode == "hidden-role-skill" {
					selected = []string{}
				}
				cfg.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"editor": {Description: "Propose improvements", SystemPrompt: "Propose a skill change without publishing it", Model: "child", Tools: []string{"skill_manage"}, Skills: selected}}}
			}
			if mode == "tool-denied" {
				cfg.ToolPolicy = harness.ToolPolicy{Denylist: []string{"skill_manage"}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			s, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "read-only" {
				if _, err = c.SetConfigOption(ctx, s.ID, "approval", harness.ApprovalReadOnly); err != nil {
					t.Fatal(err)
				}
			}
			before, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil {
				t.Fatal(err)
			}
			var emit harness.EventHandler
			if mode == "review-storage-fails" {
				_, err = c.store.DB().ExecContext(ctx, `CREATE TRIGGER fail_skill_review BEFORE INSERT ON budget_reservations WHEN NEW.kind='model' AND EXISTS (SELECT 1 FROM df_skill_proposals) BEGIN SELECT RAISE(ABORT,'fixture-review-budget-storage'); END`)
				if err != nil {
					t.Fatal(err)
				}
				emit = func(_ context.Context, event harness.RunEvent) error {
					if event.Kind == "tool_end" {
						// Remove the transient fault before the agent could continue.
						// A later successful reservation cannot erase this failure.
						_, dropErr := c.store.DB().Exec(`DROP TRIGGER IF EXISTS fail_skill_review`)
						return dropErr
					}
					return nil
				}
			}
			result, err := c.Run(ctx, s.ID, []harness.Content{{Type: "text", Text: "Improve the research skill"}}, emit, nil)
			if mode == "review-storage-fails" {
				if err == nil || !strings.Contains(err.Error(), "fixture-review-budget-storage") {
					t.Fatalf("storage failure became a successful run: %+v %v", result, err)
				}
			} else if err != nil {
				t.Fatalf("run: %+v %v", result, err)
			}
			proposalSession := s.ID
			if mode == "background-role" {
				tasks, err := c.BackgroundTasks(ctx, s.ID, "", 10)
				if err != nil || len(tasks) != 1 {
					t.Fatalf("background task: %+v %v", tasks, err)
				}
				task := tasks[0]
				for task.Status == "pending" || task.Status == "running" {
					task, err = c.WaitBackgroundTask(ctx, s.ID, task.ID, task.Version)
					if err != nil {
						t.Fatal(err)
					}
				}
				if task.Status != "completed" {
					t.Fatalf("candidate task: %+v", task)
				}
				proposalSession = task.ChildSessionID
			}
			if mode == "budget-exhausted" {
				if result.StopReason != "max_turn_requests" {
					t.Fatalf("budget result: %+v", result)
				}
			} else if mode != "review-storage-fails" && result.StopReason != "end_turn" {
				t.Fatalf("result: %+v", result)
			}
			page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			reviewCalls := calls["moderator"] + calls["evaluator"]
			totalCalls := reviewCalls + calls["parent"] + calls["child"]
			resultText := toolResult
			mu.Unlock()
			if mode == "review-storage-fails" && totalCalls != 1 {
				t.Fatalf("agent made another provider call after review infrastructure failed: %d", totalCalls)
			}
			wantProposal := mode != "hidden-role-skill" && mode != "builtin-create" && mode != "read-only" && mode != "tool-denied"
			if wantProposal {
				if len(page.Proposals) != 1 {
					t.Fatalf("candidate count=%d result=%s", len(page.Proposals), resultText)
				}
				p := page.Proposals[0]
				wantStatus := "pending_review"
				if mode == "review-fails" || mode == "review-storage-fails" || mode == "budget-exhausted" || mode == "quality-rejected" || mode == "quality-unavailable" {
					wantStatus = "failed"
				}
				if p.Status != wantStatus || p.Base != before || !strings.HasPrefix(p.Author, "model/") || p.Trigger.ThreadID != proposalSession {
					t.Fatalf("proposal: %+v", p)
				}
				if mode == "builtin-overlay" && (p.OverlayBase == nil || p.OverlayBase.Version != before.Version || p.OverlayBase.SourceID != "project") {
					t.Fatalf("model proposal lost builtin origin: %+v", p)
				}
				if mode == "review-fails" && (strings.Contains(resultText, "private-review-error") || !strings.Contains(resultText, `"status":"failed"`)) {
					t.Fatalf("review failure leaked or killed session: %s", resultText)
				}
				if mode == "quality-rejected" || mode == "quality-unavailable" {
					var feedback struct {
						Security, Quality string
						Code              string `json:"error_code"`
						Stage             string `json:"failure_stage"`
						Retryable         bool
					}
					if err := json.Unmarshal([]byte(resultText), &feedback); err != nil {
						t.Fatal(err)
					}
					wantCode, wantQuality := "quality_rejected", "failed"
					if mode == "quality-unavailable" {
						wantCode, wantQuality = "review_temporarily_unavailable", "unavailable"
					}
					if feedback.Security != "passed" || feedback.Quality != wantQuality || feedback.Code != wantCode || feedback.Stage != "quality" || feedback.Retryable != (mode == "quality-unavailable") || reviewCalls != 2 || strings.Contains(resultText, "private-review-error") {
						t.Fatalf("get lost cause, leaked diagnostics or reran reviews: %s calls=%d", resultText, reviewCalls)
					}
					if mode == "quality-rejected" && !strings.Contains(resultText, "Missing output consistency check.") {
						t.Fatalf("agent cannot revise the rejected candidate: %s", resultText)
					}
				}
			} else if len(page.Proposals) != 0 || reviewCalls != 0 {
				t.Fatalf("unauthorized candidate/model calls: %+v %d", page, reviewCalls)
			}
			if (mode == "hidden-role-skill" || mode == "builtin-create") && !strings.Contains(resultText, "permission_denied") {
				t.Fatalf("hidden skill result: %s", resultText)
			}
			if after, err := c.GetSkillHead(ctx, "project", "research"); err != nil || after != before {
				t.Fatalf("tool changed live skill: %+v %v", after, err)
			}
			if mode == "managed-create" {
				if exports, err := c.ListSkillExports(ctx, harness.SkillSelection{Workspace: workspace}); err != nil || len(exports) != 0 {
					t.Fatalf("model candidate created live export: %+v %v", exports, err)
				}
			}
			var billed int
			if err = c.store.DB().QueryRowContext(ctx, `SELECT model_calls FROM budget_roots WHERE session_id=?`, s.ID).Scan(&billed); err != nil || billed != totalCalls {
				t.Fatalf("review calls escaped root budget: billed=%d actual=%d err=%v", billed, totalCalls, err)
			}
			receipts, err := c.ListToolReceipts(ctx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, receipt := range receipts {
				want := harness.ReceiptCompleted
				if mode == "builtin-create" || mode == "hidden-role-skill" {
					want = harness.ReceiptNotExecuted
				}
				if receipt.ToolName == "skill_manage" && receipt.State != want {
					t.Fatalf("candidate receipt=%s want=%s: %+v", receipt.State, want, receipt)
				}
			}
		})
	}
}
