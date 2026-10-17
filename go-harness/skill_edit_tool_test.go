package deerflow

import (
	"context"
	"encoding/json"
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

func TestInstalledSkillEditApprovalAndRestart(t *testing.T) {
	for _, mode := range []string{"allow", "deny", "restart", "conflict", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.PermissionMode = harness.PermissionModeOff
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream   bool
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				n := calls.Add(1)
				if n == 1 {
					args := `{"action":"prepare","name":"research","edits":[{"path":"SKILL.md","operation":"replace","find":"BODY_ONLY_AFTER_LOAD","replace":"INSTALLED_UPDATE"},{"path":"guide.txt","operation":"replace","find":"PINNED_REFERENCE","replace":"UPDATED_REFERENCE"}]}`
					if mode == "invalid" {
						args = `{"action":"prepare","name":"research","edits":[{"path":"../secret","operation":"write","content":"DO_NOT_LOG_PRIVATE_PAYLOAD"}]}`
					}
					skillToolResponse(w, req.Stream, "skill_edit", args, "", "prepare-edit")
					return
				}
				if n == 2 && mode != "invalid" {
					var id string
					for _, m := range req.Messages {
						if m.Role == "tool" {
							var v struct{ Review harness.SkillEditReview }
							if json.Unmarshal([]byte(m.Content), &v) == nil {
								id = v.Review.ID
							}
						}
					}
					if id == "" {
						t.Errorf("no prepared review: %+v", req.Messages)
					}
					data, _ := json.Marshal(map[string]string{"action": "apply", "edit_id": id})
					skillToolResponse(w, req.Stream, "skill_edit", string(data), "", "apply-edit")
					return
				}
				skillToolResponse(w, req.Stream, "", "", "Edit outcome checked")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "parent", server.URL+"/v1", "fixture"
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					_ = c.Close()
				}
			}()
			s, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			before, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil {
				t.Fatal(err)
			}
			approvals := 0
			approve := func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
				if !harness.ExplicitSkillEditApproval(p) {
					return harness.AllowOnce, nil
				}
				approvals++
				var payload preparedSkillEdit
				if json.Unmarshal(p.Arguments, &payload) != nil || payload.Review == nil || len(payload.Review.Changes) != 2 || !strings.Contains(string(p.Arguments), "INSTALLED_UPDATE") {
					t.Errorf("missing authoritative approval preview: %s", p.Arguments)
				}
				if mode == "restart" {
					return harness.PermissionCancelled, nil
				}
				if mode == "deny" {
					return harness.RejectOnce, nil
				}
				if mode == "conflict" {
					if err := os.WriteFile(filepath.Join(cfg.Skills.Sources[0].Root, "research", "guide.txt"), []byte("external change"), 0600); err != nil {
						t.Error(err)
					}
				}
				return harness.AllowOnce, nil
			}
			result, err := c.Run(ctx, s.ID, []harness.Content{{Type: "text", Text: "Update the installed research skill"}}, nil, approve)
			if mode == "restart" {
				if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
					t.Fatalf("not waiting: %+v %v", result, err)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				c = nil
				c, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.LoadSession(ctx, s.ID, workspace, false, nil); err != nil {
					t.Fatal(err)
				}
				result, err = c.ResumeExecution(ctx, s.ID, harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
					if !strings.Contains(string(p.Arguments), "INSTALLED_UPDATE") {
						t.Error("resume lost review")
					}
					return harness.AllowOnce, nil
				})
			}
			if mode != "conflict" && err != nil {
				t.Fatalf("run: %+v %v", result, err)
			}
			after, e := c.GetSkillHead(ctx, "project", "research")
			if e != nil {
				t.Fatal(e)
			}
			changed := mode == "allow" || mode == "restart"
			if (after.Version != before.Version) != changed {
				t.Fatalf("head change=%t want=%t err=%v", after.Version != before.Version, changed, err)
			}
			body, e := os.ReadFile(filepath.Join(cfg.Skills.Sources[0].Root, "research", "SKILL.md"))
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(body), "INSTALLED_UPDATE") != changed {
				t.Fatal("wrong source bytes")
			}
			if mode != "invalid" && approvals != 1 {
				t.Fatalf("approval calls=%d", approvals)
			}
			var uncertain int
			if e := c.store.DB().QueryRow(`SELECT count(*) FROM harness_tool_receipts WHERE state IN ('started','uncertain')`).Scan(&uncertain); e != nil || uncertain != 0 {
				t.Fatalf("uncertain receipts=%d %v (run err=%v)", uncertain, e, err)
			}
		})
	}
}
