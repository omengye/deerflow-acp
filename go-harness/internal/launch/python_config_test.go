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
