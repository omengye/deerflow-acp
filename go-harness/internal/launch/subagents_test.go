package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
)

const roleModels = `models:
  - name: main
    provider: openai
    model: shared-model-id
    api_key: fixture-one
    base_url: https://one.invalid/v1
  - name: research
    provider: openai
    model: shared-model-id
    api_key: fixture-two
    base_url: https://two.invalid/v1
default_model: main
local_acp:
  subagent_enabled: true
  max_concurrent_subagents: 2
`

func writeRoleConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyConfigSubagentModelNamesResolveToDistinctRoutes(t *testing.T) {
	path := writeRoleConfig(t, roleModels+`subagents:
  enabled: true
  background_workers: 3
  max_depth: 2
  agents:
    general-purpose:
      delegate_to: [researcher]
  custom_agents:
    researcher:
      description: Read source material
      system_prompt: Return sources
      model: research
      tools: [web_search, read_file]
      skills: []
      max_turns: 12
      timeout_seconds: 45
`)
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Subagents == nil || cfg.DisableSubagents || cfg.BackgroundWorkers != 3 || *cfg.Subagents.MaxConcurrent != 2 || *cfg.Subagents.MaxDepth != 2 {
		t.Fatalf("subagent runtime settings not mapped: %+v", cfg.Subagents)
	}
	role := cfg.Subagents.CustomAgents["researcher"]
	if role.Model == cfg.Model || role.Model != "deerflow-config:research" || role.Skills == nil || len(role.Skills) != 0 || *role.MaxTurns != 12 || *role.TimeoutSeconds != 45 {
		t.Fatalf("role mapping failed: %+v", role)
	}
	if cfg.ModelRoutes[role.Model].BaseURL != "https://two.invalid/v1" {
		t.Fatal("role was routed to parent's provider")
	}
	cfg.BackgroundWorkers = 7
	if _, err := ApplyConfig(path, &cfg, &connections, map[string]bool{"background-workers": true}); err != nil || cfg.BackgroundWorkers != 7 {
		t.Fatalf("explicit workers lost: workers=%d err=%v", cfg.BackgroundWorkers, err)
	}
}

func TestApplyConfigSubagentGlobalDisableCannotBeExpanded(t *testing.T) {
	path := writeRoleConfig(t, roleModels+"subagents:\n  enabled: false\n")
	cfg := deerflow.Config{DisableSubagents: false}
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, map[string]bool{"disable-subagents": true}); err != nil {
		t.Fatal(err)
	}
	if !cfg.DisableSubagents {
		t.Fatal("global role disable was widened by local CLI/session default")
	}
}

func TestApplyConfigNativeSubagentGateAndCLIPrecedence(t *testing.T) {
	models, _, _ := strings.Cut(roleModels, "local_acp:")
	for _, enabled := range []string{"false", "true"} {
		path := writeRoleConfig(t, "config_version: "+configschema.NativeConfigVersion+"\n"+models+"subagents:\n  enabled: "+enabled+"\n  max_concurrent: 7\n")
		for _, flag := range []string{"absent", "false", "true"} {
			cfg := deerflow.Config{DisableSubagents: flag == "true"}
			connections := 0
			if _, err := ApplyConfig(path, &cfg, &connections, map[string]bool{"disable-subagents": flag != "absent"}); err != nil {
				t.Fatal(err)
			}
			want := enabled == "true"
			if flag != "absent" {
				want = flag == "false"
			}
			if cfg.DisableSubagents == want || cfg.Subagents.Enabled == nil || *cfg.Subagents.Enabled != want || *cfg.Subagents.MaxConcurrent != 7 {
				t.Fatalf("enabled=%s flag=%s cfg=%+v", enabled, flag, cfg.Subagents)
			}
		}
	}
}

func TestApplyConfigSubagentErrorsAreActionable(t *testing.T) {
	for _, tc := range []struct{ name, extra, field string }{
		{"typo", "subagents:\n  max_dept: 2\n", "max_dept"},
		{"unknown-model", "subagents:\n  agents:\n    general-purpose:\n      model: missing\n", ".model"},
		{"bad-timeout", "subagents:\n  timeout_seconds: 0\n", "timeout_seconds"},
		{"unsupported-thinking", "subagents:\n  agents:\n    general-purpose:\n      thinking_enabled: false\n", "thinking_enabled"},
		{"bad-thinking-budget", "subagents:\n  agents:\n    general-purpose:\n      model_settings:\n        thinking_budget_tokens: 10\n", "thinking_budget_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRoleConfig(t, roleModels+tc.extra)
			var cfg deerflow.Config
			connections := 0
			_, err := ApplyConfig(path, &cfg, &connections, nil)
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAppliedSubagentSettingsIncludeToolPolicyRestriction(t *testing.T) {
	models, _, _ := strings.Cut(roleModels, "local_acp:")
	path := writeRoleConfig(t, "config_version: "+configschema.NativeConfigVersion+"\n"+models+"subagents:\n  enabled: true\nlocal_acp:\n  tool_allowlist: []\n")
	var cfg deerflow.Config
	connections := 0
	source, err := ApplyConfig(path, &cfg, &connections, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !source.Subagents.ConfiguredEnabled || source.Subagents.Enabled || source.Subagents.ToolPolicyAllowsTask || source.Subagents.EnabledSource != "yaml" || source.Subagents.ConcurrencySource != "default" || source.Subagents.MaxConcurrent != 2 {
		t.Fatalf("untrue effective settings: %+v", source.Subagents)
	}
}

func TestApplyConfigSubagentArkOptions(t *testing.T) {
	path := writeRoleConfig(t, strings.Replace(roleModels, "provider: openai", "provider: ark", 1)+"subagents:\n  agents:\n    general-purpose:\n      model: main\n      thinking_enabled: true\n      reasoning_effort: high\n")
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "ark" || cfg.Subagents.Agents["general-purpose"].ReasoningEffort != "high" {
		t.Fatal("Ark role options were not loaded")
	}
	// Inherited roles can be selected under either model. Validate every route,
	// so choosing the OpenAI session model cannot defer failure until execution.
	path = writeRoleConfig(t, strings.Replace(roleModels, "provider: openai", "provider: ark", 1)+"subagents:\n  agents:\n    general-purpose:\n      thinking_enabled: true\n")
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err == nil || !strings.Contains(err.Error(), "thinking_enabled") {
		t.Fatalf("inherited option validation: %v", err)
	}
}
