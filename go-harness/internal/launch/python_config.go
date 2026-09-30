package launch

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	ACPAgents    map[string]any `yaml:"acp_agents"`
	DefaultModel string         `yaml:"default_model"`
	Models       []pythonModel  `yaml:"models"`
	API          struct {
		ModelName            string   `yaml:"model_name"`
		ChatRequestTimeout   *float64 `yaml:"chat_request_timeout"`
		ExtensionsConfigPath string   `yaml:"extensions_config_path"`
	} `yaml:"api"`
	LocalACP struct {
		ModelName              string    `yaml:"model_name"`
		MaxActiveConnections   *int      `yaml:"max_active_connections"`
		MaxActiveRuns          *int      `yaml:"max_active_runs"`
		QueueTimeoutSeconds    *float64  `yaml:"queue_timeout_seconds"`
		SubagentEnabled        *bool     `yaml:"subagent_enabled"`
		PermissionMode         string    `yaml:"permission_mode"`
		ToolAllowlist          *[]string `yaml:"tool_allowlist"`
		ToolDenylist           []string  `yaml:"tool_denylist"`
		GoalAutoContinue       bool      `yaml:"goal_auto_continue"`
		RunTimeoutSeconds      *float64  `yaml:"run_timeout_seconds"`
		PromptOverlay          string    `yaml:"prompt_overlay"`
		PromptOverlayFile      string    `yaml:"prompt_overlay_file"`
		EnableBash             bool      `yaml:"enable_bash"`
		AcceptClientMCPServers bool      `yaml:"accept_client_mcp_servers"`
		SessionCleanupEnabled  *bool     `yaml:"session_cleanup_enabled"`
		InactiveRetentionDays  *int      `yaml:"inactive_session_retention_days"`
		ClosedRetentionDays    *int      `yaml:"closed_session_retention_days"`
		CleanupIntervalSeconds *float64  `yaml:"session_cleanup_interval_seconds"`
	} `yaml:"local_acp"`
	Sandbox struct {
		Use           string `yaml:"use"`
		AllowHostBash bool   `yaml:"allow_host_bash"`
	} `yaml:"sandbox"`
	Skills struct {
		Enabled        *bool  `yaml:"enabled"`
		Path           string `yaml:"path"`
		ExtensionsFile string `yaml:"extensions_file"`
	} `yaml:"skills"`
	Memory struct {
		Enabled *bool `yaml:"enabled"`
	} `yaml:"memory"`
	Summarization struct {
		Enabled   bool      `yaml:"enabled"`
		ModelName string    `yaml:"model_name"`
		Trigger   yaml.Node `yaml:"trigger"`
		Keep      yaml.Node `yaml:"keep"`
	} `yaml:"summarization"`
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

func pythonToolNames(values []string) []string {
	names := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
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
	if len(source.ACPAgents) > 0 && !explicit["acp-agents-config"] {
		return result, fmt.Errorf("Python acp_agents require an explicit Go --acp-agents-config allowlist")
	}
	if source.LocalACP.EnableBash {
		if explicit["sandbox-provider"] {
			// An operator-supplied provider remains authoritative. In particular,
			// a disabled provider or argv-only local provider cannot fulfill the
			// desktop's script-capable command setting.
			if !cfg.Sandbox.Enabled || !cfg.Sandbox.AllowShell ||
				(cfg.Sandbox.Provider != harness.SandboxPowerShell && cfg.Sandbox.Provider != harness.SandboxWSL2 && cfg.Sandbox.Provider != harness.SandboxDocker) {
				return result, fmt.Errorf("local_acp.enable_bash requires an explicit Go script-capable --sandbox-provider and --sandbox-allow-shell")
			}
		} else {
			if !source.Sandbox.AllowHostBash {
				return result, fmt.Errorf("local_acp.enable_bash requires sandbox.allow_host_bash or an explicit Go --sandbox-provider")
			}
			if source.Sandbox.Use != "deerflow.sandbox.local:LocalSandboxProvider" {
				return result, fmt.Errorf("Go desktop command mapping requires sandbox.use=deerflow.sandbox.local:LocalSandboxProvider")
			}
			if runtime.GOOS != "windows" {
				return result, fmt.Errorf("automatic desktop command mapping requires Windows; select an explicit Go --sandbox-provider on this host")
			}
			if explicit["sandbox-allow-shell"] && !cfg.Sandbox.AllowShell {
				return result, fmt.Errorf("local_acp.enable_bash conflicts with explicit Go --sandbox-allow-shell=false")
			}
			// The two desktop switches jointly authorize a host command tool.
			// PowerShell is the Go local script provider on Windows. The
			// configured session permission mode and tool policy still apply.
			cfg.Sandbox.Enabled = true
			cfg.Sandbox.Provider = harness.SandboxPowerShell
			cfg.Sandbox.AllowShell = true
		}
	}
	mode := harness.PermissionMode(source.LocalACP.PermissionMode)
	if mode == "" {
		mode = harness.PermissionModeDangerous
	}
	if err := mode.Validate(); err != nil {
		return result, fmt.Errorf("local_acp.permission_mode: %w", err)
	}
	cfg.PermissionMode = mode
	policy := cfg.ToolPolicy
	if source.LocalACP.ToolAllowlist != nil {
		selected := pythonToolNames(*source.LocalACP.ToolAllowlist)
		if policy.Allowlist == nil {
			policy.Allowlist = selected
		} else {
			intersection := make([]string, 0, len(policy.Allowlist))
			for _, name := range policy.Allowlist {
				if slices.Contains(selected, name) {
					intersection = append(intersection, name)
				}
			}
			policy.Allowlist = intersection
		}
	}
	policy.Denylist = pythonToolNames(append(slices.Clone(policy.Denylist), source.LocalACP.ToolDenylist...))
	if err := policy.Validate(); err != nil {
		return result, fmt.Errorf("local_acp tool policy: %w", err)
	}
	cfg.ToolPolicy = policy
	if source.LocalACP.GoalAutoContinue {
		return result, fmt.Errorf("local_acp.goal_auto_continue has no Go equivalent")
	}
	runTimeout := 600.0
	if source.API.ChatRequestTimeout != nil {
		runTimeout = *source.API.ChatRequestTimeout
	}
	if source.LocalACP.RunTimeoutSeconds != nil {
		runTimeout = *source.LocalACP.RunTimeoutSeconds
	}
	if math.IsNaN(runTimeout) || math.IsInf(runTimeout, 0) || runTimeout <= 0 || runTimeout > 86400 {
		return result, fmt.Errorf("local_acp.run_timeout_seconds must be greater than 0 and at most 86400")
	}
	if p := source.LocalACP.MaxActiveConnections; p != nil && (*p < 1 || *p > 128) {
		return result, fmt.Errorf("local_acp.max_active_connections must be 1..128")
	}
	if p := source.LocalACP.MaxActiveRuns; p != nil && (*p < 1 || *p > 128) {
		return result, fmt.Errorf("local_acp.max_active_runs must be 1..128")
	}
	queueTimeout := 600.0
	if source.LocalACP.QueueTimeoutSeconds != nil {
		queueTimeout = *source.LocalACP.QueueTimeoutSeconds
	}
	if math.IsNaN(queueTimeout) || math.IsInf(queueTimeout, 0) || queueTimeout <= 0 || queueTimeout > 86400 {
		return result, fmt.Errorf("local_acp.queue_timeout_seconds must be greater than 0 and at most 86400")
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
	// A CLI provider or endpoint override may route requests to a different
	// service. Never carry the selected Python model's credential or advertised
	// model capabilities across that boundary.
	sameBackend := cfg.Provider == provider && cfg.BaseURL == baseURL
	if sameBackend && apiKey != "" && cfg.APIKey == "" {
		cfg.APIKey = apiKey
	}
	if !explicit["data-dir"] && cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(filepath.Dir(abs), "go-harness-state")
	}
	if !explicit["max-connections"] {
		*maxConnections = 16
		if source.LocalACP.MaxActiveConnections != nil {
			*maxConnections = *source.LocalACP.MaxActiveConnections
		}
	}
	if !explicit["max-active-runs"] {
		cfg.MaxActiveRuns = 2
		if source.LocalACP.MaxActiveRuns != nil {
			cfg.MaxActiveRuns = *source.LocalACP.MaxActiveRuns
		}
	}
	if !explicit["queue-timeout"] {
		cfg.QueueTimeout = time.Duration(queueTimeout * float64(time.Second))
	}
	if !explicit["disable-subagents"] {
		cfg.DisableSubagents = source.LocalACP.SubagentEnabled == nil || !*source.LocalACP.SubagentEnabled
	}
	if !explicit["run-timeout"] {
		if cfg.Budget == nil {
			budget := harness.DefaultBudgetLimits()
			cfg.Budget = &budget
		}
		cfg.Budget.Timeout = time.Duration(runTimeout * float64(time.Second))
	}
	overlay := strings.TrimSpace(source.LocalACP.PromptOverlay)
	if source.LocalACP.PromptOverlayFile != "" {
		overlayPath := source.LocalACP.PromptOverlayFile
		if !filepath.IsAbs(overlayPath) {
			overlayPath = filepath.Join(filepath.Dir(abs), overlayPath)
		}
		file, readErr := os.Open(overlayPath)
		if readErr != nil {
			return result, fmt.Errorf("read local_acp.prompt_overlay_file: %w", readErr)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 4<<20+1))
		readErr = errors.Join(readErr, file.Close())
		if readErr != nil {
			return result, fmt.Errorf("read local_acp.prompt_overlay_file: %w", readErr)
		}
		if len(data) > 4<<20 || !utf8.Valid(data) {
			return result, fmt.Errorf("local_acp.prompt_overlay_file must be UTF-8 and at most 4 MiB")
		}
		overlay = strings.TrimSpace(string(data))
	}
	if utf8.RuneCountInString(overlay) > 65536 {
		return result, fmt.Errorf("local_acp.prompt_overlay must be at most 65536 characters")
	}
	if overlay != "" {
		base := strings.TrimSpace(cfg.Instruction)
		if base != "" {
			base += "\n"
		}
		cfg.Instruction = base + "<deployment_instructions>\n" + overlay + "\n</deployment_instructions>"
	}
	if source.Memory.Enabled != nil {
		cfg.MemoryEnabled = source.Memory.Enabled
		if !explicit["memory-extraction"] {
			cfg.MemoryExtraction = *source.Memory.Enabled
		} else if cfg.MemoryExtraction {
			// An explicit Go extraction flag also opts the Go model back into
			// access to its own memory store.
			enabled := true
			cfg.MemoryEnabled = &enabled
		}
	}
	if err := applyPythonSummarization(source, cfg, explicit); err != nil {
		return result, err
	}
	if err := applyPythonSkills(abs, source, cfg, explicit); err != nil {
		return result, err
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
		if !sameBackend || option.Model == "" || option.Use != model.Use {
			continue
		}
		key, keyErr := pythonScalar(option.APIKey)
		url, urlErr := pythonScalar(option.BaseURL)
		if keyErr != nil || urlErr != nil || key != cfg.APIKey || url != baseURL {
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

type pythonContextSize struct {
	Type  string  `yaml:"type"`
	Value float64 `yaml:"value"`
}

func decodePythonContextSizes(node yaml.Node) ([]pythonContextSize, error) {
	if node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	var values []pythonContextSize
	if node.Kind == yaml.SequenceNode {
		if err := node.Decode(&values); err != nil {
			return nil, err
		}
	} else if node.Kind == yaml.MappingNode {
		var value pythonContextSize
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		values = []pythonContextSize{value}
	} else {
		return nil, fmt.Errorf("context size must be an object or list")
	}
	return values, nil
}

func applyPythonSummarization(source pythonRuntimeConfig, cfg *deerflow.Config, explicit map[string]bool) error {
	setting := source.Summarization
	if !setting.Enabled {
		if !explicit["context-compaction"] {
			cfg.Compaction.Enabled = false
		}
		return nil
	}
	if strings.TrimSpace(setting.ModelName) != "" {
		return fmt.Errorf("summarization.model_name requires an explicit Go compaction model adapter")
	}
	values, err := decodePythonContextSizes(setting.Trigger)
	if err != nil {
		return fmt.Errorf("summarization.trigger: %w", err)
	}
	if len(values) == 0 || len(values) > 8 {
		return fmt.Errorf("summarization.trigger must contain 1..8 supported thresholds")
	}
	// Go's Eino middleware accepts both message and token thresholds. Use its
	// supported maxima for an omitted dimension so a single Python trigger does
	// not acquire an earlier, unrelated threshold.
	messages, tokens := 10_000, 2_000_000
	for _, value := range values {
		if math.IsNaN(value.Value) || math.IsInf(value.Value, 0) || value.Value != math.Trunc(value.Value) {
			return fmt.Errorf("summarization.trigger must use whole messages or tokens")
		}
		switch value.Type {
		case "messages":
			if value.Value < 1 || value.Value > 10_000 {
				return fmt.Errorf("summarization.trigger messages must be 1..10000")
			}
			messages = min(messages, int(value.Value))
		case "tokens":
			if value.Value < 1 || value.Value > 2_000_000 {
				return fmt.Errorf("summarization.trigger tokens must be 1..2000000")
			}
			tokens = min(tokens, int(value.Value))
		default:
			return fmt.Errorf("summarization.trigger %q is not supported by Go compaction", value.Type)
		}
	}
	keep := 20
	if setting.Keep.Kind != 0 && setting.Keep.Tag != "!!null" {
		values, err = decodePythonContextSizes(setting.Keep)
		if err != nil || len(values) != 1 || values[0].Type != "messages" || math.IsNaN(values[0].Value) || math.IsInf(values[0].Value, 0) || values[0].Value != math.Trunc(values[0].Value) || values[0].Value < 1 || values[0].Value > 1000 {
			return fmt.Errorf("summarization.keep must be 1..1000 messages")
		}
		keep = int(values[0].Value)
	}
	if messages <= keep+1 {
		return fmt.Errorf("summarization.trigger messages must exceed kept messages by at least two")
	}
	if !explicit["context-compaction"] {
		cfg.Compaction.Enabled = true
	}
	if !explicit["compact-after-messages"] {
		cfg.Compaction.ContextMessages = messages
	}
	if !explicit["compact-after-tokens"] {
		cfg.Compaction.ContextTokens = tokens
	}
	if !explicit["compact-keep-messages"] {
		cfg.Compaction.KeepRecentMessages = keep
	}
	return nil
}

func resolvePythonConfigPath(configPath, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(configPath), value))
}

func pythonSkillStates(configPath string, source pythonRuntimeConfig) (map[string]bool, error) {
	name := source.API.ExtensionsConfigPath
	if name == "" {
		name = source.Skills.ExtensionsFile
	}
	if name == "" {
		name = "./extensions_config.json"
	}
	file, err := os.Open(resolvePythonConfigPath(configPath, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Skills extension settings: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("Skills extension settings exceed 1 MiB or cannot be read")
	}
	var document struct {
		Skills map[string]struct {
			Enabled *bool `json:"enabled"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse Skills extension settings: %w", err)
	}
	states := make(map[string]bool, len(document.Skills))
	for name, state := range document.Skills {
		if state.Enabled != nil {
			states[name] = *state.Enabled
		}
	}
	return states, nil
}

func pythonSkillName(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	// Allow for CRLF expansion of the registry's 16 KiB normalized header.
	data, err := io.ReadAll(io.LimitReader(file, 32<<10+1))
	if err != nil {
		return ""
	}
	// Windows portable packages check out SKILL.md with CRLF. The registry
	// accepts either form, so discovery must use the same normalized view.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return ""
	}
	ending := strings.Index(text[4:], "\n---\n")
	if ending < 0 {
		return ""
	}
	var metadata struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal([]byte(text[4:4+ending]), &metadata) != nil {
		return ""
	}
	return metadata.Name
}

func applyPythonSkills(configPath string, source pythonRuntimeConfig, cfg *deerflow.Config, explicit map[string]bool) error {
	if explicit["skills-config"] {
		return nil
	}
	if source.Skills.Enabled != nil && !*source.Skills.Enabled {
		cfg.Skills = harness.SkillsConfig{}
		cfg.SkillSelection = harness.SkillSelection{Names: []string{}}
		return nil
	}
	if source.Skills.Path == "" {
		if source.Skills.Enabled != nil && *source.Skills.Enabled {
			return fmt.Errorf("skills.enabled requires an explicit skills.path for Go")
		}
		return nil
	}
	root := resolvePythonConfigPath(configPath, source.Skills.Path)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skills.path must name an existing ordinary directory")
	}
	states, err := pythonSkillStates(configPath, source)
	if err != nil {
		return err
	}
	config := harness.SkillsConfig{}
	for _, category := range []string{"public", "custom"} {
		categoryRoot := filepath.Join(root, category)
		info, err = os.Lstat(categoryRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skills %s directory must be an ordinary directory", category)
		}
		id := "python-" + category
		config.Sources = append(config.Sources, harness.SkillSource{ID: id, Root: categoryRoot, Scope: harness.SkillScopeGlobal})
		entries := 0
		err = filepath.WalkDir(categoryRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			entries++
			if entries > 4096 {
				return fmt.Errorf("skills %s exceeds 4096 directory entries", category)
			}
			if entry.IsDir() || entry.Name() != "SKILL.md" || !entry.Type().IsRegular() {
				return nil
			}
			dir := filepath.Dir(path)
			name := pythonSkillName(path)
			if name == "" || filepath.Base(dir) != name {
				// Python can load packages whose directory differs from their name;
				// Go's immutable Skills registry intentionally rejects them.
				return nil
			}
			rel, err := filepath.Rel(categoryRoot, dir)
			if err != nil {
				return err
			}
			enabled, known := states[name]
			if !known {
				enabled = true
			}
			config.Install = append(config.Install, harness.SkillInstall{SourceID: id, Directory: filepath.ToSlash(rel), Enabled: enabled})
			if len(config.Install) > 128 {
				return fmt.Errorf("Skills configuration exceeds 128 installations")
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("inspect skills %s: %w", category, err)
		}
	}
	sort.Slice(config.Install, func(i, j int) bool {
		if config.Install[i].SourceID != config.Install[j].SourceID {
			return config.Install[i].SourceID < config.Install[j].SourceID
		}
		return config.Install[i].Directory < config.Install[j].Directory
	})
	cfg.Skills = config
	cfg.SkillSelection = harness.SkillSelection{IncludeGlobal: true}
	return nil
}
