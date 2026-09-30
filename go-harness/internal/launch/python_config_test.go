package launch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
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

func TestApplyPythonConfigDesktopHostCommandGates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	config := func(enable, host bool, provider string) {
		t.Helper()
		raw := fmt.Sprintf(`local_acp:
  enable_bash: %t
sandbox:
  use: %s
  allow_host_bash: %t
models:
  - name: one
    use: langchain_openai:ChatOpenAI
    model: one
`, enable, provider, host)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const local = "deerflow.sandbox.local:LocalSandboxProvider"
	apply := func(cfg *deerflow.Config, explicit map[string]bool) error {
		t.Helper()
		connections := 16
		_, err := ApplyPythonConfig(path, cfg, &connections, explicit)
		return err
	}
	config(false, false, local)
	var cfg deerflow.Config
	if err := apply(&cfg, nil); err != nil || cfg.Sandbox.Enabled {
		t.Fatalf("default unexpectedly enabled host commands: %+v err=%v", cfg.Sandbox, err)
	}
	config(false, true, local)
	cfg = deerflow.Config{}
	if err := apply(&cfg, nil); err != nil || cfg.Sandbox.Enabled {
		t.Fatalf("host consent alone enabled command tool: %+v err=%v", cfg.Sandbox, err)
	}
	config(true, false, local)
	cfg = deerflow.Config{}
	if err := apply(&cfg, nil); err == nil || !strings.Contains(err.Error(), "sandbox.allow_host_bash") {
		t.Fatalf("ACP switch without host consent: %+v err=%v", cfg.Sandbox, err)
	}
	config(true, true, "third.party:Provider")
	cfg = deerflow.Config{}
	if err := apply(&cfg, nil); err == nil || !strings.Contains(err.Error(), "sandbox.use") {
		t.Fatalf("unrecognized local provider enabled commands: %+v err=%v", cfg.Sandbox, err)
	}
	config(true, true, local)
	cfg = deerflow.Config{}
	err := apply(&cfg, nil)
	if runtime.GOOS == "windows" {
		if err != nil || !cfg.Sandbox.Enabled || cfg.Sandbox.Provider != harness.SandboxPowerShell || !cfg.Sandbox.AllowShell || !cfg.PermissionMode.RequiresPermission("execute") {
			t.Fatalf("two-switch Go desktop command mapping: %+v err=%v", cfg.Sandbox, err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "requires Windows") {
		t.Fatalf("non-Windows implicit host shell accepted: %+v err=%v", cfg.Sandbox, err)
	}
	cfg = deerflow.Config{Sandbox: harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true, Shell: "operator-shell"}}
	if err := apply(&cfg, map[string]bool{"sandbox-provider": true}); err != nil || cfg.Sandbox.Provider != harness.SandboxPowerShell || !cfg.Sandbox.AllowShell || cfg.Sandbox.Shell != "operator-shell" {
		t.Fatalf("Python config overrode explicit Go script provider: %+v err=%v", cfg.Sandbox, err)
	}
	cfg = deerflow.Config{Sandbox: harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{"/operator/git"}}}
	if err := apply(&cfg, map[string]bool{"sandbox-provider": true}); err == nil || !strings.Contains(err.Error(), "script-capable") {
		t.Fatalf("argv-only provider accepted Bash setting: %+v err=%v", cfg.Sandbox, err)
	}
	cfg = deerflow.Config{Sandbox: harness.SandboxConfig{Provider: harness.SandboxDisabled}}
	if err := apply(&cfg, map[string]bool{"sandbox-provider": true}); err == nil || !strings.Contains(err.Error(), "script-capable") || cfg.Sandbox.Enabled {
		t.Fatalf("explicit Go disabled provider accepted Bash setting: %+v err=%v", cfg.Sandbox, err)
	}
	cfg = deerflow.Config{Sandbox: harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell}}
	if err := apply(&cfg, map[string]bool{"sandbox-provider": true}); err == nil || !strings.Contains(err.Error(), "sandbox-allow-shell") {
		t.Fatalf("explicit Go provider without shell consent accepted Bash: %+v err=%v", cfg.Sandbox, err)
	}
	if runtime.GOOS == "windows" {
		cfg = deerflow.Config{}
		if err := apply(&cfg, map[string]bool{"sandbox-allow-shell": true}); err == nil || !strings.Contains(err.Error(), "sandbox-allow-shell=false") {
			t.Fatalf("Python config overrode explicit shell denial: %+v err=%v", cfg.Sandbox, err)
		}
	}
}

func TestApplyPythonConfigDesktopHostCommandRuns(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("desktop's implicit PowerShell mapping is Windows-only")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `local_acp:
  enable_bash: true
sandbox:
  use: deerflow.sandbox.local:LocalSandboxProvider
  allow_host_bash: true
models:
  - name: one
    use: langchain_openai:ChatOpenAI
    model: one
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 16
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.New(context.Background(), cfg.Sandbox, t.TempDir())
	if err != nil {
		t.Fatalf("mapped desktop command backend: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, err := backend.Start(ctx, harness.CommandRequest{Script: `[Console]::Write('desktop-shell-ready')`})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := backend.Wait(ctx, started.ID)
	if err != nil || finished.Stdout.Text != "desktop-shell-ready" || finished.ExitCode == nil || *finished.ExitCode != 0 || !finished.TerminationConfirmed {
		t.Fatalf("mapped desktop command result: %+v err=%v", finished, err)
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
		"max_active_runs: 0", "max_active_runs: 129",
		"queue_timeout_seconds: 0", "queue_timeout_seconds: 86401", "queue_timeout_seconds: .nan",
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

func TestApplyPythonConfigDesktopHarnessFeatures(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	public := filepath.Join(root, "skills", "public", "research")
	custom := filepath.Join(root, "skills", "custom", "writer")
	for _, dir := range []string{configDir, public, custom, filepath.Join(root, "skills", "public", "alias")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, name := range map[string]string{
		filepath.Join(public, "SKILL.md"):                            "research",
		filepath.Join(custom, "SKILL.md"):                            "writer",
		filepath.Join(root, "skills", "public", "alias", "SKILL.md"): "different-name",
	} {
		content := "---\nname: " + name + "\ndescription: A test skill\n---\n\nInstructions.\n"
		if name == "research" {
			content = strings.ReplaceAll(content, "\n", "\r\n")
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configDir, "extensions.json"), []byte(`{"skills":{"writer":{"enabled":false},"research":{"enabled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "config.yaml")
	raw := `api:
  extensions_config_path: extensions.json
local_acp:
  max_active_runs: 3
  queue_timeout_seconds: 2.5
skills:
  enabled: true
  path: ../skills
memory:
  enabled: true
summarization:
  enabled: true
  trigger:
    - {type: messages, value: 80}
    - {type: tokens, value: 30000}
  keep: {type: messages, value: 20}
models:
  - name: one
    use: langchain_openai:ChatOpenAI
    model: one
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxActiveRuns != 3 || cfg.QueueTimeout != 2500*time.Millisecond || cfg.MemoryEnabled == nil || !*cfg.MemoryEnabled || !cfg.MemoryExtraction || !cfg.Compaction.Enabled || cfg.Compaction.ContextMessages != 80 || cfg.Compaction.ContextTokens != 30000 || cfg.Compaction.KeepRecentMessages != 20 {
		t.Fatalf("runtime features were not mapped: %+v", cfg)
	}
	if len(cfg.Skills.Sources) != 2 || len(cfg.Skills.Install) != 2 || !cfg.SkillSelection.IncludeGlobal {
		t.Fatalf("Skills source/installation mapping: %+v selection=%+v", cfg.Skills, cfg.SkillSelection)
	}
	installed := make(map[string]bool)
	for _, skill := range cfg.Skills.Install {
		installed[skill.Directory] = skill.Enabled
	}
	if !installed["research"] || installed["writer"] || len(installed) != 2 {
		t.Fatalf("Skills extension state or incompatible package handling: %+v", cfg.Skills.Install)
	}
	// An explicit Go profile remains authoritative over every mapped knob.
	cfg.MaxActiveRuns = 5
	cfg.QueueTimeout = 9 * time.Second
	cfg.MemoryExtraction = false
	cfg.Compaction = harness.CompactionConfig{Enabled: false, ContextMessages: 40, ContextTokens: 40000, KeepRecentMessages: 8}
	cfg.Skills = harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "host", Root: root, Scope: harness.SkillScopeGlobal}}}
	cfg.SkillSelection = harness.SkillSelection{IncludeGlobal: false, Names: []string{}}
	_, err := ApplyPythonConfig(path, &cfg, &connections, map[string]bool{
		"max-active-runs": true, "queue-timeout": true, "memory-extraction": true,
		"context-compaction": true, "compact-after-messages": true, "compact-after-tokens": true,
		"compact-keep-messages": true, "skills-config": true,
	})
	if err != nil || cfg.MaxActiveRuns != 5 || cfg.QueueTimeout != 9*time.Second || cfg.MemoryExtraction || cfg.Compaction.Enabled || cfg.Compaction.ContextMessages != 40 || cfg.Compaction.ContextTokens != 40000 || cfg.Compaction.KeepRecentMessages != 8 || len(cfg.Skills.Sources) != 1 || cfg.Skills.Sources[0].ID != "host" || cfg.SkillSelection.IncludeGlobal {
		t.Fatalf("explicit Go settings lost: config=%+v err=%v", cfg, err)
	}
}

func TestApplyPythonConfigDisabledMemoryAndExplicitExtraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("memory:\n  enabled: false\nmodels:\n  - name: one\n    use: langchain_openai:ChatOpenAI\n    model: one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil || cfg.MemoryEnabled == nil || *cfg.MemoryEnabled || cfg.MemoryExtraction {
		t.Fatalf("disabled memory mapping: %+v err=%v", cfg, err)
	}
	cfg.MemoryExtraction = true
	if _, err := ApplyPythonConfig(path, &cfg, &connections, map[string]bool{"memory-extraction": true}); err != nil || cfg.MemoryEnabled == nil || !*cfg.MemoryEnabled || !cfg.MemoryExtraction {
		t.Fatalf("explicit Go extraction override: %+v err=%v", cfg, err)
	}
}

func TestApplyPythonConfigRejectsUnsupportedSummarization(t *testing.T) {
	for _, yamlBody := range []string{
		"  enabled: true\n",
		"  enabled: true\n  model_name: another\n  trigger: {type: messages, value: 80}\n",
		"  enabled: true\n  trigger: {type: fraction, value: 0.8}\n",
		"  enabled: true\n  trigger: {type: messages, value: 12}\n  keep: {type: messages, value: 20}\n",
		"  enabled: true\n  trigger: {type: messages, value: 80}\n  keep: {type: tokens, value: 1000}\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		raw := "summarization:\n" + yamlBody + "models:\n  - name: one\n    use: langchain_openai:ChatOpenAI\n    model: one\n"
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var cfg deerflow.Config
		connections := 0
		if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err == nil || !strings.Contains(err.Error(), "summarization") {
			t.Fatalf("unsupported summarization accepted: %q err=%v", yamlBody, err)
		}
	}
}

func TestApplyCurrentExampleSkillsStartGoHarness(t *testing.T) {
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyPythonConfig(filepath.Join("..", "..", "..", "config.example.yaml"), &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills.Install) != 22 {
		t.Fatalf("expected all 22 bundled Skills, got %d", len(cfg.Skills.Install))
	}
	cfg.DataDir = t.TempDir()
	cfg.APIKey = "fixture"
	client, err := deerflow.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("bundled Skills prevent Go startup: %v", err)
	}
	listed, err := client.ListSkills(context.Background(), cfg.SkillSelection)
	if err != nil || len(listed) != 22 {
		t.Fatalf("bundled Skills were not installed: count=%d err=%v", len(listed), err)
	}
	var vercel bool
	for _, record := range listed {
		if record.Ref.Name == "vercel-deploy" && record.Enabled {
			vercel = true
		}
	}
	if !vercel {
		t.Fatal("bundled Vercel deployment Skill is missing or disabled")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}
