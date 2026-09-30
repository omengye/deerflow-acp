package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestApplyPythonConfigModelAndPortableSettings(t *testing.T) {
	t.Setenv("FIXTURE_MODEL_KEY", "from-environment")
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `default_model: other
api:
  model_name: other
local_acp:
  model_name: selected
  max_active_connections: 7
  run_timeout_seconds: 33
  subagent_enabled: false
  session_cleanup_enabled: false
  inactive_session_retention_days: 7
  closed_session_retention_days: 0
  session_cleanup_interval_seconds: 120
models:
  - name: other
    use: langchain_openai:ChatOpenAI
    model: other-id
  - name: selected
    display_name: Selected model
    use: langchain_openai:ChatOpenAI
    model: real-model-id
    api_key: $FIXTURE_MODEL_KEY
    base_url: https://example.test/v1
    supports_vision: true
    context_window: 131072
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	limits := harness.DefaultBudgetLimits()
	cfg := deerflow.Config{Budget: &limits}
	connections := 32
	result, err := ApplyPythonConfig(path, &cfg, &connections, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != path || len(result.Revision) != 64 || cfg.Provider != "openai" || cfg.Model != "real-model-id" || cfg.APIKey != "from-environment" || cfg.BaseURL != "https://example.test/v1" || cfg.DataDir != filepath.Join(filepath.Dir(path), "go-harness-state") || !cfg.DisableSubagents || connections != 7 || cfg.Budget.Timeout != 33*time.Second {
		t.Fatalf("config=%+v connections=%d result=%+v", cfg, connections, result)
	}
	if len(cfg.Models) != 1 || cfg.Models[0].Name != "Selected model" || len(cfg.Media.VisionModels) != 1 || cfg.Media.VisionModels[0] != "real-model-id" || cfg.ContextWindows["real-model-id"] != 131072 {
		t.Fatalf("model capabilities: %+v %+v", cfg.Models, cfg.Media)
	}
	if cfg.Retention.Enabled || cfg.Retention.InactiveDays != 7 || cfg.Retention.ClosedDays != 0 || cfg.Retention.CheckInterval != 2*time.Minute {
		t.Fatalf("retention mapping: %+v", cfg.Retention)
	}
	cfg.Model, cfg.Provider, cfg.BaseURL, cfg.DataDir = "flag-model", "claude", "https://flag.test", "flag-state"
	connections = 2
	cfg.Retention = harness.RetentionPolicy{Enabled: true, InactiveDays: 90, ClosedDays: 60, CheckInterval: time.Hour}
	_, err = ApplyPythonConfig(path, &cfg, &connections, map[string]bool{"model": true, "provider": true, "base-url": true, "data-dir": true, "max-connections": true, "disable-subagents": true, "session-cleanup-enabled": true, "inactive-session-retention-days": true, "closed-session-retention-days": true, "session-cleanup-interval": true})
	if err != nil || cfg.Model != "flag-model" || cfg.Provider != "claude" || cfg.BaseURL != "https://flag.test" || cfg.DataDir != "flag-state" || connections != 2 {
		t.Fatalf("explicit flags lost: %+v %d %v", cfg, connections, err)
	}
	if !cfg.Retention.Enabled || cfg.Retention.InactiveDays != 90 || cfg.Retention.ClosedDays != 60 || cfg.Retention.CheckInterval != time.Hour {
		t.Fatalf("explicit retention lost: %+v", cfg.Retention)
	}
	explicitWindow := deerflow.Config{Model: "real-model-id", ContextWindow: 2048}
	_, err = ApplyPythonConfig(path, &explicitWindow, &connections, map[string]bool{"model": true, "context-window": true})
	if err != nil || explicitWindow.ContextWindow != 2048 || explicitWindow.ContextWindows["real-model-id"] != 0 {
		t.Fatalf("explicit context window lost: %+v err=%v", explicitWindow, err)
	}
}

func TestApplyPythonConfigRejectsUnsupportedAuthority(t *testing.T) {
	for _, extra := range []string{"  enable_bash: true\n", "  accept_client_mcp_servers: true\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		raw := "local_acp:\n" + extra + "models:\n  - name: one\n    use: langchain_openai:ChatOpenAI\n    model: one\n"
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var cfg deerflow.Config
		connections := 0
		if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("unsupported authority accepted: %v", err)
		}
	}
}

func TestApplyPythonConfigToolPolicyIntersectsHostBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `local_acp:
  tool_allowlist: [" read_file ", "task", "read_file", ""]
  tool_denylist: ["task", " write_file ", "write_file"]
models:
  - name: one
    use: langchain_openai:ChatOpenAI
    model: one
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 32
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.ToolPolicy.Allowlist == nil || !cfg.ToolPolicy.Allows("read_file") || cfg.ToolPolicy.Allows("task") || cfg.ToolPolicy.Allows("write_file") || cfg.ToolPolicy.Allows("search_files") {
		t.Fatalf("Python tool policy=%+v", cfg.ToolPolicy)
	}
	cfg.ToolPolicy = harness.ToolPolicy{Allowlist: []string{"read_file", "search_files"}, Denylist: []string{"read_file"}}
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.ToolPolicy.Allowlist == nil || cfg.ToolPolicy.Allows("read_file") || cfg.ToolPolicy.Allows("search_files") || cfg.ToolPolicy.Allows("task") {
		t.Fatalf("Python config widened existing Go tool policy: %+v", cfg.ToolPolicy)
	}
	if err := os.WriteFile(path, []byte(`local_acp:
  tool_allowlist: []
models:
  - name: one
    use: langchain_openai:ChatOpenAI
    model: one
`), 0600); err != nil {
		t.Fatal(err)
	}
	var empty deerflow.Config
	if _, err := ApplyPythonConfig(path, &empty, &connections, nil); err != nil || empty.ToolPolicy.Allowlist == nil || empty.ToolPolicy.Allows("read_file") {
		t.Fatalf("empty allowlist did not disable all tools: %+v err=%v", empty.ToolPolicy, err)
	}
}

func TestApplyPythonConfigPermissionModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		value string
		want  harness.PermissionMode
	}{
		{"", harness.PermissionModeDangerous},
		{"off", harness.PermissionModeOff},
		{"dangerous", harness.PermissionModeDangerous},
		{"all", harness.PermissionModeAll},
	} {
		raw := "local_acp:\n  permission_mode: " + tc.value + "\nmodels:\n  - name: one\n    use: langchain_openai:ChatOpenAI\n    model: one\n"
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var cfg deerflow.Config
		connections := 1
		if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil || cfg.PermissionMode != tc.want {
			t.Fatalf("mode=%q mapped=%q err=%v", tc.value, cfg.PermissionMode, err)
		}
	}
	if err := os.WriteFile(path, []byte("local_acp:\n  permission_mode: unknown\nmodels:\n  - name: one\n    use: langchain_openai:ChatOpenAI\n    model: one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 1
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err == nil {
		t.Fatal("accepted an unknown Python permission mode")
	}
}

func TestApplyPythonConfigDoesNotCarryCredentialsAcrossBackendOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `local_acp:
  model_name: selected
models:
  - name: selected
    use: langchain_openai:ChatOpenAI
    model: python-model
    api_key: python-secret
    base_url: https://python.example/v1
    supports_vision: true
    context_window: 100000
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		provider string
		baseURL  string
	}{
		{"provider", "claude", "https://python.example/v1"},
		{"endpoint", "openai", "https://other.example/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := deerflow.Config{Provider: tc.provider, Model: "host-model", BaseURL: tc.baseURL, APIKey: "host-secret"}
			connections := 2
			_, err := ApplyPythonConfig(path, &cfg, &connections, map[string]bool{"provider": true, "model": true, "base-url": true})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.APIKey != "host-secret" || len(cfg.Models) != 0 || len(cfg.Media.VisionModels) != 0 || len(cfg.ContextWindows) != 0 {
				t.Fatalf("Python backend metadata crossed %s override: %+v", tc.name, cfg)
			}
		})
	}
	cfg := deerflow.Config{Provider: "openai", Model: "host-model", BaseURL: "https://python.example/v1"}
	connections := 2
	_, err := ApplyPythonConfig(path, &cfg, &connections, map[string]bool{"provider": true, "model": true, "base-url": true})
	if err != nil || cfg.APIKey != "python-secret" || len(cfg.Models) != 1 {
		t.Fatalf("same backend lost compatible Python options: %+v %v", cfg, err)
	}
	cfg = deerflow.Config{Provider: "openai", Model: "host-model", BaseURL: "https://python.example/v1", APIKey: "host-secret"}
	_, err = ApplyPythonConfig(path, &cfg, &connections, map[string]bool{"provider": true, "model": true, "base-url": true})
	if err != nil || cfg.APIKey != "host-secret" || len(cfg.Models) != 0 || len(cfg.Media.VisionModels) != 0 || len(cfg.ContextWindows) != 0 {
		t.Fatalf("existing Go credential was replaced: %+v %v", cfg, err)
	}
}

func TestApplyPythonConfigRejectsInvalidRetention(t *testing.T) {
	for _, field := range []string{
		"inactive_session_retention_days: 0",
		"closed_session_retention_days: -1",
		"session_cleanup_interval_seconds: 30",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		raw := "local_acp:\n  " + field + "\nmodels:\n  - name: one\n    use: langchain_openai:ChatOpenAI\n    model: one\n"
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var cfg deerflow.Config
		connections := 32
		if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err == nil {
			t.Fatalf("invalid retention accepted: %s", field)
		}
	}
}

func TestApplyCurrentExamplePythonConfig(t *testing.T) {
	var cfg deerflow.Config
	connections := 32
	_, err := ApplyPythonConfig(filepath.Join("..", "..", "..", "config.example.yaml"), &cfg, &connections, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "qwen3.6-plus" || cfg.Provider != "openai" || !cfg.DisableSubagents || connections != 16 || cfg.ContextWindows["qwen3.6-plus"] != 262144 {
		t.Fatalf("example config mapping: %+v connections=%d", cfg, connections)
	}
}

func TestApplyPythonConfigDefaultsAndPromptOverlay(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(filepath.Join(root, "instructions.md"), []byte("  File-owned instruction.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := `api:
  chat_request_timeout: 7.5
local_acp:
  prompt_overlay: Inline instruction.
  prompt_overlay_file: instructions.md
models:
  - name: one
    use: langchain_openai:ChatOpenAI
    model: one
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := deerflow.Config{Instruction: "Base instruction."}
	connections := 32
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if connections != 16 || !cfg.DisableSubagents || cfg.Budget == nil || cfg.Budget.Timeout != 7500*time.Millisecond {
		t.Fatalf("Python defaults not mapped: connections=%d subagents=%v budget=%+v", connections, cfg.DisableSubagents, cfg.Budget)
	}
	if cfg.Instruction != "Base instruction.\n<deployment_instructions>\nFile-owned instruction.\n</deployment_instructions>" {
		t.Fatalf("prompt overlay not applied with file precedence: %q", cfg.Instruction)
	}
	budget := harness.DefaultBudgetLimits()
	budget.Timeout = 12 * time.Second
	cfg = deerflow.Config{Instruction: "Base instruction.", Budget: &budget}
	connections = 5
	_, err := ApplyPythonConfig(path, &cfg, &connections, map[string]bool{"run-timeout": true, "max-connections": true, "disable-subagents": true})
	if err != nil || cfg.Budget.Timeout != 12*time.Second || connections != 5 || cfg.DisableSubagents {
		t.Fatalf("explicit Go flags lost: connections=%d subagents=%v budget=%+v err=%v", connections, cfg.DisableSubagents, cfg.Budget, err)
	}
}

func TestApplyPythonConfigRejectsInvalidPortableBounds(t *testing.T) {
	for _, local := range []string{
		"run_timeout_seconds: 0", "run_timeout_seconds: -1", "run_timeout_seconds: .nan",
		"max_active_connections: 0", "max_active_connections: 129",
		"prompt_overlay_file: missing.md",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		raw := "local_acp:\n  " + local + "\nmodels:\n  - name: one\n    use: langchain_openai:ChatOpenAI\n    model: one\n"
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var cfg deerflow.Config
		connections := 32
		if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err == nil {
			t.Fatalf("accepted invalid Python setting %q", local)
		}
	}
}
