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
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Reproduce the desktop failure through the model/tool/receipt path, then
// correct only the reference arguments and reuse the original prepared edit.
func TestInstalledSkillEditReferenceNameRecovery(t *testing.T) {
	for _, action := range []string{"apply", "status"} {
		t.Run(action, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.PermissionMode = harness.PermissionModeOff
			path := filepath.Join(workspace, "skills", "research", "guide.txt")
			var calls, approvals atomic.Int32
			var editID string
			var prepared, applied bool
			unchanged := func() {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != "PINNED_REFERENCE" {
					t.Errorf("rejected arguments changed installed file: %q %v", body, err)
				}
				if approvals.Load() != 0 {
					t.Error("rejected arguments or status requested application approval")
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream   bool
					Messages []struct{ Role, Content string }
					Tools    []struct {
						Function struct {
							Name, Description string
							Parameters        struct {
								Properties map[string]struct{ Description string }
							}
						}
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				last := ""
				for _, message := range req.Messages {
					if message.Role == "tool" {
						last = message.Content
					}
				}
				switch calls.Add(1) {
				case 1:
					found := false
					for _, tool := range req.Tools {
						if tool.Function.Name != "skill_edit" {
							continue
						}
						found = true
						for _, field := range []string{"name", "edits"} {
							if !strings.Contains(tool.Function.Parameters.Properties[field].Description, "Omit for apply/status") {
								t.Errorf("wire schema does not explain %s restriction", field)
							}
						}
						if !strings.Contains(tool.Function.Description, `{"action":"apply","edit_id":"<edit_id>"}`) || !strings.Contains(tool.Function.Description, `{"action":"status","edit_id":"<edit_id>"}`) {
							t.Error("wire tool description lacks reference examples")
						}
					}
					if !found {
						t.Error("skill_edit schema missing")
					}
					skillToolResponse(w, req.Stream, "skill_edit", `{"action":"prepare","name":"research","edits":[{"path":"guide.txt","operation":"replace","find":"PINNED_REFERENCE","replace":"UPDATED_REFERENCE"}]}`, "", "prepare-edit")
				case 2:
					var result struct {
						Status string
						Review harness.SkillEditReview
					}
					if json.Unmarshal([]byte(last), &result) != nil || result.Status != "prepared" || result.Review.ID == "" {
						t.Errorf("prepare failed: %s", last)
					}
					editID, prepared = result.Review.ID, result.Status == "prepared"
					args, _ := json.Marshal(map[string]string{"action": action, "edit_id": editID, "name": "research"})
					skillToolResponse(w, req.Stream, "skill_edit", string(args), "", "invalid-reference")
				case 3:
					if !strings.Contains(last, "Remove name") || !strings.Contains(last, "Retry the same edit_id") || strings.Contains(last, "file format, match count") {
						t.Errorf("model cannot correct its arguments: %s", last)
					}
					unchanged()
					skillToolResponse(w, req.Stream, "skill_edit", fmt.Sprintf(`{"action":"status","edit_id":%q}`, editID), "", "correct-status")
				case 4:
					var state harness.SkillEditResult
					if json.Unmarshal([]byte(last), &state) != nil || state.Status != "prepared" || state.Review.ID != editID {
						t.Errorf("corrected status lost the original edit: %s", last)
					}
					unchanged()
					skillToolResponse(w, req.Stream, "skill_edit", fmt.Sprintf(`{"action":"apply","edit_id":%q}`, editID), "", "correct-apply")
				default:
					var state harness.SkillEditResult
					applied = json.Unmarshal([]byte(last), &state) == nil && state.Status == "applied" && state.LiveMatches && state.SourceMatches && state.Review.ID == editID
					if !applied {
						t.Errorf("corrected apply failed: %s", last)
					}
					skillToolResponse(w, req.Stream, "", "", "Edit verified")
				}
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			client, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			var events []harness.RunEvent
			_, err = client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Update the installed research skill"}}, func(_ context.Context, event harness.RunEvent) error {
				events = append(events, event)
				return nil
			}, func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
				if harness.ExplicitSkillEditApproval(request) {
					approvals.Add(1)
					var payload preparedSkillEdit
					if json.Unmarshal(request.Arguments, &payload) != nil || payload.Review == nil || payload.ID != editID || payload.Name != "" || !strings.Contains(string(request.Arguments), "UPDATED_REFERENCE") {
						t.Error("corrected apply lacks its authoritative approval preview")
					}
				}
				return harness.AllowOnce, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !prepared || !applied || calls.Load() != 5 || approvals.Load() != 1 {
				t.Fatalf("recovery: prepared=%v applied=%v calls=%d approvals=%d", prepared, applied, calls.Load(), approvals.Load())
			}
			start, rejected := false, false
			for _, event := range events {
				if event.ToolCallID != "invalid-reference" {
					continue
				}
				if event.Kind == "tool_start" {
					start = true
					if string(event.Arguments) != "{}" {
						t.Errorf("rejected raw arguments entered the receipt: %s", event.Arguments)
					}
				}
				if event.Kind == "tool_end" {
					rejected = event.Status == "failed" && event.Receipt != nil && event.Receipt.State == "not_executed" && event.Receipt.FailureCode == "invalid_skill_edit_arguments"
				}
			}
			if !start || !rejected {
				t.Fatal("missing classified not-executed receipt")
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != "UPDATED_REFERENCE" {
				t.Fatalf("corrected edit was not installed: %q %v", body, err)
			}
		})
	}
}
