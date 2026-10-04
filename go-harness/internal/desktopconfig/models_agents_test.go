package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestNativeModelAndRoleSettingsRoundTrip(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	first := modelByName(t, doc, "first")
	first["provider"], first["api_key"] = "claude", "replacement-key"
	first["context_window"], first["supports_vision"] = 131072, true
	doc["default_model"] = "second"
	roles := readObject(doc, "subagents")
	roles["max_concurrent"] = 3
	roles["enabled"], roles["auto_continue"] = true, true
	roles["max_turns"], roles["timeout_seconds"], roles["max_depth"] = 17, 12.5, 3
	roles["background_workers"], roles["max_tasks_per_root"], roles["max_auto_continuations"] = 2, 9, 4
	roles["agents"] = map[string]any{"general-purpose": map[string]any{"delegate_to": []string{"reviewer"}}}
	roles["custom_agents"] = map[string]any{"reviewer": map[string]any{
		"description": "Review source evidence", "system_prompt": "Return evidence and uncertainties.", "model": "first",
		"tools": []string{}, "skills": []string{}, "disallowed_tools": []string{"bash"}, "delegate_to": []string{},
		"max_turns": 4, "timeout_seconds": 5.5, "thinking_enabled": true,
		"model_settings": map[string]any{"temperature": 1, "max_tokens": 4096, "thinking_budget_tokens": 2048},
	}}
	saved := run(t, options, "save", doc)
	run(t, options, "validate", saved)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openai" || cfg.Model != "gpt-b" || cfg.DisableSubagents || cfg.Subagents == nil {
		t.Fatal("root routing or role gate changed")
	}
	c := cfg.Subagents
	if *c.MaxConcurrent != 3 || *c.MaxTurns != 17 || *c.TimeoutSeconds != 12.5 || *c.MaxDepth != 3 || cfg.BackgroundWorkers != 2 || *c.MaxTasksPerRoot != 9 || !*c.AutoContinue || *c.MaxAutoContinuations != 4 {
		t.Fatal("global role settings not consumed")
	}
	role := c.CustomAgents["reviewer"]
	route, ok := cfg.ModelRoutes[role.Model]
	if !ok || route.Provider != "claude" || route.APIKey != "replacement-key" || cfg.ContextWindows[role.Model] != 131072 || len(cfg.Media.VisionModels) != 1 || cfg.Media.VisionModels[0] != role.Model {
		t.Fatal("role provider/context/vision route not consumed")
	}
	if role.ThinkingEnabled == nil || !*role.ThinkingEnabled || *role.ModelSettings.ThinkingBudgetTokens != 2048 || *role.ModelSettings.MaxTokens != 4096 || *role.ModelSettings.Temperature != 1 {
		t.Fatal("native generation options lost")
	}
	registry, err := agents.New(*c, map[string]bool{cfg.Model: true, role.Model: true})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := registry.Resolve("reviewer", agents.Parent{Model: cfg.Model, Tools: harness.ToolPolicy{Allowlist: []string{"read_file"}}, Skills: []string{"example"}, MaxTurns: 10, TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != role.Model || spec.MaxTurns != 4 || spec.TimeoutSeconds != 5.5 || spec.Tools.Allowlist == nil || len(spec.Tools.Allowlist) != 0 || spec.Skills == nil || len(spec.Skills) != 0 {
		t.Fatal("role factory did not preserve explicit empty grants and role limits")
	}
	if builtin, err := registry.Resolve("general-purpose", agents.Parent{Model: cfg.Model, Tools: harness.ToolPolicy{Allowlist: []string{"read_file"}}, Skills: []string{"example"}, MaxTurns: 20, TimeoutSeconds: 20}); err != nil || len(builtin.DelegateTo) != 1 || builtin.DelegateTo[0] != "reviewer" || len(builtin.Tools.Allowlist) != 1 || len(builtin.Skills) != 1 {
		t.Fatalf("inherited role grants changed: %v", err)
	}
}

func TestNativeRoleSaveRejectsSameInvalidConfigAsLaunch(t *testing.T) {
	role := func() map[string]any {
		return map[string]any{"description": "Review", "system_prompt": "Use evidence", "model": "inherit"}
	}
	for _, name := range []string{"unknown-field", "unknown-model", "unknown-builtin", "cycle", "provider-options", "duplicate-tools", "credential", "depth", "nested-unknown", "fractional-turns", "tiny-timeout", "unicode-bytes", "thinking-type"} {
		t.Run(name, func(t *testing.T) {
			options := setup(t)
			doc := run(t, options, "snapshot", nil)
			subagents := readObject(doc, "subagents")
			r := role()
			subagents["custom_agents"] = map[string]any{"reviewer": r}
			switch name {
			case "unknown-field":
				subagents["advanced"] = map[string]any{}
			case "unknown-model":
				r["model"] = "missing"
			case "unknown-builtin":
				subagents["agents"] = map[string]any{"general": map[string]any{}}
			case "cycle":
				r["delegate_to"] = []string{"reviewer"}
			case "provider-options":
				r["thinking_enabled"] = true
			case "duplicate-tools":
				r["tools"] = []string{"read_file", "read_file"}
			case "credential":
				r["api_key"] = "role-private-fixture"
			case "depth":
				subagents["max_depth"] = 4
			case "nested-unknown":
				r["model_settings"] = map[string]any{"unknown": true}
			case "fractional-turns":
				r["max_turns"] = 2.5
			case "tiny-timeout":
				r["timeout_seconds"] = .0005
			case "unicode-bytes":
				r["description"] = strings.Repeat("界", 1366)
			case "thinking-type":
				r["thinking_enabled"] = "false"
			}
			before, _ := os.ReadFile(options.Config)
			_, err := Run(options, "save", mustJSON(t, doc))
			if err == nil || strings.Contains(err.Error(), "role-private-fixture") {
				t.Fatalf("invalid role was accepted or echoed credentials: %v", err)
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid role changed configuration")
			}
			raw, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			raw["subagents"] = subagents
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			var cfg deerflow.Config
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err == nil {
				t.Fatal("launch accepted rejected role config")
			}
			if name == "credential" {
				visible := mustJSON(t, run(t, options, "snapshot", nil))
				if bytes.Contains(visible, []byte("role-private-fixture")) {
					t.Fatal("invalid source credential exposed in snapshot")
				}
			}
		})
	}
}

func TestNativeModelProjectionPreservesUnknownOriginalsWithoutEditor(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	model := list(raw, "models")[0].(map[string]any)
	model["supports_thinking"], model["vendor_extension"] = true, map[string]any{"api_key": "vendor-private-fixture"}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	for _, key := range []string{"advanced", "use_path", "supports_thinking", "supports_reasoning_effort", "vendor_extension"} {
		if _, exists := modelByName(t, doc, "first")[key]; exists {
			t.Fatalf("snapshot exposes %s as editable", key)
		}
	}
	if bytes.Contains(mustJSON(t, doc), []byte("vendor-private-fixture")) {
		t.Fatal("snapshot exposed original extension credential")
	}
	saved := run(t, options, "save", doc)
	raw, err = readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if str(readObject(list(raw, "models")[0].(map[string]any), "vendor_extension"), "api_key", "") != "vendor-private-fixture" {
		t.Fatal("unrelated save changed original extension")
	}
	modelByName(t, saved, "first")["base_url"] = "https://different.invalid/v1"
	run(t, options, "save", saved)
	raw, err = readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := list(raw, "models")[0].(map[string]any)["vendor_extension"]; exists {
		t.Fatal("backend change transferred hidden extension")
	}
	for _, key := range []string{"advanced", "supports_thinking", "use_path"} {
		doc = run(t, options, "snapshot", nil)
		modelByName(t, doc, "first")[key] = true
		if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
			t.Fatalf("accepted old model editor field %s", key)
		}
	}
}

func TestLegacyMainAgentDirectoryIsNotReadWrittenOrBackedUp(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	dir := filepath.Join(options.UserData, "data", "deerflow", "agents", "legacy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	original := []byte("intentionally invalid yaml: [\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	if _, exists := doc["agents"]; exists {
		t.Fatal("legacy main agents still in snapshot")
	}
	saved := run(t, options, "save", doc)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("native save changed legacy agent directory")
	}
	if _, err := os.Stat(filepath.Join(str(saved, "backup", ""), "agents")); !os.IsNotExist(err) {
		t.Fatal("native save still scans/copies legacy agents")
	}
	saved["agents"] = []any{}
	if _, err := Run(options, "save", mustJSON(t, saved)); err == nil {
		t.Fatal("accepted removed legacy agent mutation")
	}
}

func TestNativeModelRenameAndClearConsumedAPIAlias(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	object(raw, "api")["model_name"] = "second"
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	if readObject(doc, "runtime")["model_name"] != "second" {
		t.Fatal("effective API model alias is hidden")
	}
	modelByName(t, doc, "second")["name"] = "renamed"
	readObject(doc, "runtime")["model_name"] = "renamed"
	saved := run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.Model != "gpt-b" {
		t.Fatalf("renamed alias not consumed: %v", err)
	}
	readObject(saved, "runtime")["model_name"] = nil
	run(t, options, "save", saved)
	cfg = deerflow.Config{}
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.Model != "gpt-a" {
		t.Fatalf("cleared alias still shadows default: %v", err)
	}
}
