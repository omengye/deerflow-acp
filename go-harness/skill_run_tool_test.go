package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillrun"
)

func TestSkillRunNativePermissionPinnedReceiptAndReadOnly(t *testing.T) {
	py, err := skillrun.Interpreter(harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true})
	if err != nil {
		t.Skip(err)
	}
	for _, mode := range []string{"run", "denied", "read-only", "plan", "execute-denied", "forged-version"} {
		t.Run(mode, func(t *testing.T) {
			ws, cfg := skillFixture(t)
			script := filepath.Join(ws, "skills", "research", "run.py")
			if err := os.WriteFile(script, []byte("from pathlib import Path\nPath('effect.txt').write_text('pinned')\nprint('done')\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.Sandbox = harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{py}, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": py}, Limits: harness.CommandLimits{Timeout: 10 * time.Second}}
			cfg.PermissionMode = harness.PermissionModeDangerous
			if mode == "execute-denied" {
				cfg.ToolPolicy = harness.ToolPolicy{Denylist: []string{"execute"}}
			}
			calls := 0
			result := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream   bool
					Tools    []struct{ Function struct{ Name string } }
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				calls++
				if calls == 1 {
					visible := false
					for _, tool := range req.Tools {
						visible = visible || tool.Function.Name == "skill_run"
					}
					want := mode != "read-only" && mode != "plan" && mode != "execute-denied"
					if visible != want {
						t.Errorf("visibility %t expected %t", visible, want)
					}
					if !want {
						skillToolResponse(w, req.Stream, "", "", "No command available")
						return
					}
					args := `{"action":"run","name":"research","path":"run.py"}`
					if mode == "forged-version" {
						args = `{"action":"run","name":"research","path":"run.py","pinned_version":"forged"}`
					}
					// Source mutation after snapshot selection must not alter execution.
					if err := os.WriteFile(script, []byte("raise Exception('mutable version')"), 0600); err != nil {
						t.Error(err)
					}
					skillToolResponse(w, req.Stream, "skill_run", args, "")
					return
				}
				for _, m := range req.Messages {
					if m.Role == "tool" {
						result += m.Content
					}
				}
				skillToolResponse(w, req.Stream, "", "", "Finished")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			client, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession(ctx, ws)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "read-only" {
				_, err = client.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalReadOnly)
			}
			if mode == "plan" {
				err = client.SetMode(ctx, session.ID, "plan")
			}
			if err != nil {
				t.Fatal(err)
			}
			permissions := 0
			events := []harness.RunEvent{}
			_, err = client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Execute the packaged fixture"}}, func(_ context.Context, e harness.RunEvent) error { events = append(events, e); return nil }, func(_ context.Context, r harness.PermissionRequest) (harness.PermissionDecision, error) {
				permissions++
				if r.ToolName != "skill_run" || !strings.Contains(string(r.Arguments), `"pinned_version"`) {
					t.Errorf("approval lacks pinned identity: %+v", r)
				}
				if mode == "denied" {
					return harness.RejectOnce, nil
				}
				return harness.AllowOnce, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			content, readErr := os.ReadFile(filepath.Join(ws, "effect.txt"))
			if mode == "run" {
				if readErr != nil || string(content) != "pinned" || !strings.Contains(result, `"execution_checked":true`) || permissions != 1 {
					t.Fatal("execution/receipt", readErr, result, permissions)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal("unauthorized effect", mode, readErr)
			}
			if mode == "forged-version" && permissions != 0 {
				t.Fatal("model forged host field reached approval")
			}
		})
	}
}

func TestSkillRunDelegatedRoleCannotExpandToolOrSkillScope(t *testing.T) {
	py, err := skillrun.Interpreter(harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true})
	if err != nil {
		t.Skip(err)
	}
	for _, mode := range []string{"allowed", "hidden-skill", "no-execute"} {
		t.Run(mode, func(t *testing.T) {
			ws, cfg := skillFixture(t)
			if err := os.WriteFile(filepath.Join(ws, "skills", "research", "run.py"), []byte("from pathlib import Path\nPath('role-effect').touch()\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.Sandbox = harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{py}, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": py}}
			cfg.DisableSubagents = false
			cfg.PermissionMode = harness.PermissionModeOff
			calls := map[string]int{}
			var toolResults []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string
					Stream   bool
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				calls[req.Model]++
				for _, m := range req.Messages {
					if m.Role == "tool" {
						toolResults = append(toolResults, m.Content)
					}
				}
				if calls[req.Model] == 1 {
					if req.Model == "parent" {
						skillToolResponse(w, req.Stream, "task", `{"subagent_type":"runner","prompt":"Run the research helper","description":"Run helper"}`, "")
						return
					}
					skillToolResponse(w, req.Stream, "skill_run", `{"action":"run","name":"research","path":"run.py"}`, "")
					return
				}
				skillToolResponse(w, req.Stream, "", "", "Complete")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "parent", server.URL+"/v1", "fixture"
			cfg.ModelRoutes = map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child", BaseURL: server.URL + "/v1", APIKey: "fixture"}}
			tools := []string{"skill_run", "execute"}
			selected := []string{"research"}
			if mode == "hidden-skill" {
				selected = []string{}
			}
			if mode == "no-execute" {
				tools = []string{"skill_run"}
			}
			cfg.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"runner": {Description: "Run a pinned skill", SystemPrompt: "Use only permitted skill helpers", Model: "child", Tools: tools, Skills: selected}}}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			s, err := c.NewSession(ctx, ws)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Run(ctx, s.ID, []harness.Content{{Type: "text", Text: "Delegate the permitted skill helper"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(filepath.Join(ws, "role-effect"))
			if (err == nil) != (mode == "allowed") {
				t.Fatal("delegated authority mismatch", mode, err, calls, toolResults)
			}
		})
	}
}
