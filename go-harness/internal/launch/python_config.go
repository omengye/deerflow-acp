package launch

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

// PythonConfig is the deliberately small, checked adapter for a Bridge
// --config file. Paths to Python SQLite stores are never reused by Go.
type PythonConfig struct {
	Path     string
	Revision string
}

type pythonModel struct {
	Name           string `yaml:"name"`
	DisplayName    string `yaml:"display_name"`
	Use            string `yaml:"use"`
	Model          string `yaml:"model"`
	APIKey         string `yaml:"api_key"`
	BaseURL        string `yaml:"base_url"`
	SupportsVision bool   `yaml:"supports_vision"`
	ContextWindow  int    `yaml:"context_window"`
}

type pythonRuntimeConfig struct {
	DefaultModel string        `yaml:"default_model"`
	Models       []pythonModel `yaml:"models"`
	API          struct {
		ModelName string `yaml:"model_name"`
	} `yaml:"api"`
	LocalACP struct {
		ModelName              string    `yaml:"model_name"`
		MaxActiveConnections   int       `yaml:"max_active_connections"`
		SubagentEnabled        *bool     `yaml:"subagent_enabled"`
		PermissionMode         string    `yaml:"permission_mode"`
		ToolAllowlist          *[]string `yaml:"tool_allowlist"`
		ToolDenylist           []string  `yaml:"tool_denylist"`
		GoalAutoContinue       bool      `yaml:"goal_auto_continue"`
		RunTimeoutSeconds      int       `yaml:"run_timeout_seconds"`
		EnableBash             bool      `yaml:"enable_bash"`
		AcceptClientMCPServers bool      `yaml:"accept_client_mcp_servers"`
		SessionCleanupEnabled  *bool     `yaml:"session_cleanup_enabled"`
		InactiveRetentionDays  *int      `yaml:"inactive_session_retention_days"`
		ClosedRetentionDays    *int      `yaml:"closed_session_retention_days"`
		CleanupIntervalSeconds *float64  `yaml:"session_cleanup_interval_seconds"`
	} `yaml:"local_acp"`
}

func pythonProvider(use string) (string, error) {
	switch use {
	case "langchain_openai:ChatOpenAI":
		return "openai", nil
	case "langchain_anthropic:ChatAnthropic":
		return "claude", nil
	default:
		return "", fmt.Errorf("unsupported Python model provider %q", use)
	}
}

func pythonScalar(value string) (string, error) {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name := value[2 : len(value)-1]
		if key, fallback, ok := strings.Cut(name, ":-"); ok {
			if current := os.Getenv(key); current != "" {
				return current, nil
			}
			return fallback, nil
		}
		value = "$" + name
	}
	if strings.HasPrefix(value, "$") {
		name := strings.TrimPrefix(value, "$")
		if name == "" || strings.ContainsAny(name, "${} ") {
			return "", fmt.Errorf("invalid environment reference")
		}
		current, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s required by config is missing", name)
		}
		return current, nil
	}
	return value, nil
}

// ApplyPythonConfig maps model selection and portable daemon settings from
// config.yaml. Explicit Go CLI flags win; unsupported executable/MCP policies
// fail when enabled instead of silently widening Go's authority.
func ApplyPythonConfig(path string, cfg *deerflow.Config, maxConnections *int, explicit map[string]bool) (PythonConfig, error) {
	var result PythonConfig
	abs, err := filepath.Abs(path)
	if err != nil {
		return result, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return result, err
	}
	file, err := os.Open(abs)
	if err != nil {
		return result, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 2<<20+1))
	if err != nil {
		return result, err
	}
	if len(raw) > 2<<20 {
		return result, fmt.Errorf("Python config exceeds 2 MiB")
	}
	var source pythonRuntimeConfig
	if err = yaml.Unmarshal(raw, &source); err != nil {
		return result, fmt.Errorf("parse Python config: %w", err)
	}
	if source.LocalACP.EnableBash && (!explicit["sandbox-provider"] || !cfg.Sandbox.Enabled || !cfg.Sandbox.AllowShell) {
		return result, fmt.Errorf("local_acp.enable_bash requires an explicit Go --sandbox-provider and shell policy")
	}
	if source.LocalACP.PermissionMode != "" && source.LocalACP.PermissionMode != "dangerous" {
		return result, fmt.Errorf("local_acp.permission_mode %q has no equivalent Go policy", source.LocalACP.PermissionMode)
	}
	if source.LocalACP.ToolAllowlist != nil || len(source.LocalACP.ToolDenylist) != 0 {
		return result, fmt.Errorf("local_acp tool allow/deny lists require a Go policy migration")
	}
	if source.LocalACP.GoalAutoContinue {
		return result, fmt.Errorf("local_acp.goal_auto_continue has no Go equivalent")
	}
	if source.LocalACP.RunTimeoutSeconds < 0 || source.LocalACP.RunTimeoutSeconds > 86400 {
		return result, fmt.Errorf("local_acp.run_timeout_seconds must be 0..86400")
	}
	if source.LocalACP.MaxActiveConnections < 0 || source.LocalACP.MaxActiveConnections > 1024 {
		return result, fmt.Errorf("local_acp.max_active_connections must be 0..1024")
	}
	if p := source.LocalACP.InactiveRetentionDays; p != nil && (*p < 1 || *p > 3650) {
		return result, fmt.Errorf("local_acp.inactive_session_retention_days must be 1..3650")
	}
	if p := source.LocalACP.ClosedRetentionDays; p != nil && (*p < 0 || *p > 3650) {
		return result, fmt.Errorf("local_acp.closed_session_retention_days must be 0..3650")
	}
	if p := source.LocalACP.CleanupIntervalSeconds; p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 60 || *p > 86400) {
		return result, fmt.Errorf("local_acp.session_cleanup_interval_seconds must be 60..86400")
	}
	if source.LocalACP.AcceptClientMCPServers && len(cfg.MCP.AllowedCommands) == 0 && !cfg.MCP.AllowHTTP && !cfg.MCP.AllowSSE {
		return result, fmt.Errorf("local_acp.accept_client_mcp_servers requires explicit Go MCP allow flags")
	}
	selected := source.LocalACP.ModelName
	if selected == "" {
		selected = source.API.ModelName
	}
	if selected == "" {
		selected = source.DefaultModel
	}
	if selected == "" && len(source.Models) > 0 {
		selected = source.Models[0].Name
	}
	if selected == "" {
		return result, fmt.Errorf("Python config has no selected model")
	}
	var model *pythonModel
	for i := range source.Models {
		if source.Models[i].Name == selected {
			model = &source.Models[i]
			break
		}
	}
	if model == nil || model.Model == "" {
		return result, fmt.Errorf("selected Python model %q is missing or has no model ID", selected)
	}
	provider, err := pythonProvider(model.Use)
	if err != nil {
		return result, err
	}
	apiKey, err := pythonScalar(model.APIKey)
	if err != nil {
		return result, err
	}
	baseURL, err := pythonScalar(model.BaseURL)
	if err != nil {
		return result, err
	}
	if !explicit["provider"] {
		cfg.Provider = provider
	}
	if !explicit["model"] {
		cfg.Model = model.Model
	}
	if !explicit["base-url"] && baseURL != "" {
		cfg.BaseURL = baseURL
	}
	if apiKey != "" {
		cfg.APIKey = apiKey
	}
	if !explicit["data-dir"] && cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(filepath.Dir(abs), "go-harness-state")
	}
	if !explicit["max-connections"] && source.LocalACP.MaxActiveConnections > 0 {
		*maxConnections = source.LocalACP.MaxActiveConnections
	}
	if !explicit["disable-subagents"] && source.LocalACP.SubagentEnabled != nil {
		cfg.DisableSubagents = !*source.LocalACP.SubagentEnabled
	}
	if !explicit["run-timeout"] && source.LocalACP.RunTimeoutSeconds > 0 && cfg.Budget != nil {
		cfg.Budget.Timeout = time.Duration(source.LocalACP.RunTimeoutSeconds) * time.Second
	}
	if p := source.LocalACP.SessionCleanupEnabled; p != nil && !explicit["session-cleanup-enabled"] {
		cfg.Retention.Enabled = *p
	}
	if p := source.LocalACP.InactiveRetentionDays; p != nil && !explicit["inactive-session-retention-days"] {
		cfg.Retention.InactiveDays = *p
	}
	if p := source.LocalACP.ClosedRetentionDays; p != nil && !explicit["closed-session-retention-days"] {
		cfg.Retention.ClosedDays = *p
	}
	if p := source.LocalACP.CleanupIntervalSeconds; p != nil && !explicit["session-cleanup-interval"] {
		cfg.Retention.CheckInterval = time.Duration(*p * float64(time.Second))
	}
	for _, option := range source.Models {
		if option.Model == "" || option.Use != model.Use {
			continue
		}
		key, keyErr := pythonScalar(option.APIKey)
		url, urlErr := pythonScalar(option.BaseURL)
		if keyErr != nil || urlErr != nil || key != apiKey || url != baseURL {
			continue
		}
		if option.ContextWindow < 0 || option.ContextWindow > 1<<30 {
			return result, fmt.Errorf("invalid context_window for model %q", option.Name)
		}
		name := option.DisplayName
		if name == "" {
			name = option.Name
		}
		cfg.Models = append(cfg.Models, harness.ConfigValue{Value: option.Model, Name: name})
		if option.ContextWindow > 0 && !(explicit["context-window"] && option.Model == cfg.Model) {
			if cfg.ContextWindows == nil {
				cfg.ContextWindows = make(map[string]int)
			}
			cfg.ContextWindows[option.Model] = option.ContextWindow
		}
		if option.SupportsVision {
			cfg.Media.VisionModels = append(cfg.Media.VisionModels, option.Model)
		}
	}
	result.Path = abs
	result.Revision = fmt.Sprintf("%x", sha256.Sum256(raw))
	return result, nil
}
