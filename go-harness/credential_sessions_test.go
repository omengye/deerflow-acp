package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillrun"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

// Exercise real model/tool schemas, session policy, command dispatch, admission
// masking and disk-backed reopen. The form supplies only synthetic test values.
func TestDesktopCredentialNewSessionOnlyConfirmsUse(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	user, key := randomTestSecret(t, "user-"), randomTestSecret(t, "key-")
	var prompts int
	ctx := secrets.WithCredentialPrompt(context.Background(), func(_ context.Context, s secrets.CredentialScope, _ secrets.CredentialFields) (secrets.CredentialPair, error) {
		prompts++
		if s.Consumer != "tool:execute" || s.Service != "https://service.example" || s.Account != "default" {
			t.Fatal("wrong credential destination")
		}
		return secrets.CredentialPair{Username: []byte(user), Key: []byte(key)}, nil
	})
	provider, seen := secretProvider(t, []secretToolStep{{"execute", map[string]any{"executable": exe, "args": []string{"-test.run=^TestSecretCommandFixtureChild$"}, "credential": map[string]string{"service": "https://SERVICE.example:443/", "username_env": "FIXTURE_OTHER", "key_env": "FIXTURE_TOKEN"}}}})
	root := t.TempDir()
	ws := testutil.CanonicalTempDir(t)
	cfg := Config{DataDir: root, Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeOff, DisableSubagents: true, Sandbox: harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}, Environment: map[string]string{"DEERFLOW_SECRET_COMMAND_FIXTURE": "1"}, Limits: harness.CommandLimits{Timeout: 30 * time.Second}}}
	for i := 0; i < 3; i++ {
		c, err := Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer c.Close()
			session, err := c.NewSession(ctx, ws)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalAllowAlways); err != nil {
				t.Fatal(err)
			}
			prompt := "Use the service."
			if i == 1 {
				prompt += " Known pasted key " + key
			}
			approvals, executed := 0, 0
			_, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, func(_ context.Context, e harness.RunEvent) error {
				data, _ := json.Marshal(e)
				if bytes.Contains(data, []byte(user)) || bytes.Contains(data, []byte(key)) {
					t.Error("credential appeared in session event")
				}
				if e.Kind == "tool_execute" && e.ToolName == "execute" {
					executed++
				}
				return nil
			}, func(_ context.Context, r harness.PermissionRequest) (harness.PermissionDecision, error) {
				if r.ToolName != "execute" || !harness.ExplicitSecretApproval(r) {
					t.Error("unexpected permission request")
				}
				approvals++
				if i == 2 {
					return harness.RejectOnce, nil
				}
				return harness.AllowOnce, nil
			})
			if err != nil || approvals != 1 || (executed == 1) != (i != 2) || prompts != 1 {
				t.Fatalf("session %d approvals=%d executions=%d form entries=%d err=%v", i+1, approvals, executed, prompts, err)
			}
		}()
	}
	bodies, reqs := seen()
	for _, body := range bodies {
		if bytes.Contains(body, []byte(key)) || bytes.Contains(body, []byte(user)) {
			t.Fatal("form value appeared in a model request")
		}
	}
	var masked bool
	for _, r := range reqs {
		for _, v := range r.toolResults() {
			if strings.Contains(v, "token=[secret:DF_CREDENTIAL_") && strings.Contains(v, "other=[secret:DF_CREDENTIAL_") {
				masked = true
			}
		}
	}
	if !masked {
		t.Fatal("expected both injected fields in masked command output")
	}
	assertNoSecretBytes(t, root, user, key)
}

func TestDesktopCredentialSkillApprovalBindsOptionalAcrossResume(t *testing.T) {
	py, err := skillrun.Interpreter(harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true})
	if err != nil {
		t.Skip(err)
	}
	ws, cfg := skillFixture(t)
	root := filepath.Join(ws, "skills", "research")
	manifest := `{"version":1,"python":{"min_version":"3.8","entrypoints":["run.py"]},"secrets":[{"name":"APP_USER"},{"name":"APP_KEY"},{"name":"LATER","required":false},{"name":"REFRESH","write":true,"required":false}]}`
	if err = os.WriteFile(filepath.Join(root, "skill-runtime.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	script := "import os,pathlib\nassert os.environ['APP_USER'] and os.environ['APP_KEY']\nwith pathlib.Path('used.txt').open('a') as f: f.write('Y' if 'LATER' in os.environ else 'N')\n"
	if err = os.WriteFile(filepath.Join(root, "run.py"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	provider, seen := secretProvider(t, []secretToolStep{{"skill_run", map[string]any{"action": "run", "name": "research", "path": "run.py", "credential": map[string]string{"service": "https://skill.example", "username_env": "APP_USER", "key_env": "APP_KEY"}}}})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	cfg.Sandbox = harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{py}, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": py}}
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
	cfg.PermissionMode = harness.PermissionModeOff
	prompts := 0
	user, key := randomTestSecret(t, "user-"), randomTestSecret(t, "key-")
	ctx := secrets.WithCredentialPrompt(context.Background(), func(context.Context, secrets.CredentialScope, secrets.CredentialFields) (secrets.CredentialPair, error) {
		prompts++
		return secrets.CredentialPair{Username: []byte(user), Key: []byte(key)}, nil
	})
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 2; i++ {
		session, err := c.NewSession(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalAllowAlways); err != nil {
			t.Fatal(err)
		}
		confirmations := 0
		_, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Use the service from the research skill."}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
			confirmations++
			var approved preparedSkillRun
			if p.ToolName != "skill_run" || json.Unmarshal(p.Arguments, &approved) != nil || approved.SecretPlan == nil || approved.SecretPlan.Credential == nil || len(approved.SecretPlan.Write) != 1 || approved.SecretPlan.Write[0] != "REFRESH" || (approved.SecretPlan.Read["LATER"] != "") != (i == 1) {
				t.Error("incorrect scope on skill approval")
			}
			if i == 0 {
				_, err := c.secrets.Set(ctx, secrets.SetRequest{Name: "LATER", Kind: secrets.KindString, Value: []byte("later-test-value")})
				if err != nil {
					return harness.RejectOnce, err
				}
			}
			return harness.AllowOnce, nil
		})
		if err != nil || confirmations != 1 || prompts != 1 {
			t.Fatalf("session %d, confirmations %d, entries %d: %v", i+1, confirmations, prompts, err)
		}
	}
	effect, err := os.ReadFile(filepath.Join(ws, "used.txt"))
	if err != nil || string(effect) != "NY" {
		t.Fatal("skill used a different set from its approval", err)
	}
	bodies, _ := seen()
	for _, body := range bodies {
		if bytes.Contains(body, []byte(user)) || bytes.Contains(body, []byte(key)) {
			t.Fatal("skill login reached model")
		}
	}
}
