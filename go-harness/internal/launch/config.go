package launch

import (
	"context"
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
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"go.yaml.in/yaml/v3"
)

// ConfigSource identifies the native configuration file loaded by the daemon.
// Its digest lets the desktop distinguish saved configuration from applied state.
type ConfigSource struct {
	Path      string
	Revision  string
	Subagents AppliedSubagentSettings
	Settings  AppliedSettings
}

type modelDocument struct {
	Name                 string                       `yaml:"name"`
	DisplayName          string                       `yaml:"display_name"`
	Use                  string                       `yaml:"use"`
	Provider             string                       `yaml:"provider"`
	Model                string                       `yaml:"model"`
	APIKey               string                       `yaml:"api_key"`
	BaseURL              string                       `yaml:"base_url"`
	SupportsVision       bool                         `yaml:"supports_vision"`
	ContextWindow        int                          `yaml:"context_window"`
	ReasoningEfforts     []string                     `yaml:"reasoning_efforts"`
	ThinkingParameter    string                       `yaml:"thinking_parameter"`
	ThinkingBudgetTokens int                          `yaml:"thinking_budget_tokens"`
	OutputTokenParameter harness.OutputTokenParameter `yaml:"output_token_parameter"`
}

type runtimeDocument struct {
	WeChat         deerflow.WeChatConfig       `yaml:"wechat"`
	SkillEvolution yaml.Node                   `yaml:"skill_evolution"`
	Subagents      yaml.Node                   `yaml:"subagents"`
	ACPAgents      map[string]any              `yaml:"acp_agents"`
	DefaultModel   string                      `yaml:"default_model"`
	Models         []modelDocument             `yaml:"models"`
	Tools          []harness.BuiltinToolConfig `yaml:"tools"`
	API            struct {
		ModelName            string   `yaml:"model_name"`
		ChatRequestTimeout   *float64 `yaml:"chat_request_timeout"`
		ExtensionsConfigPath string   `yaml:"extensions_config_path"`
	} `yaml:"api"`
	LocalACP struct {
		ModelName              string    `yaml:"model_name"`
		MaxActiveConnections   *int      `yaml:"max_active_connections"`
		MaxActiveRuns          *int      `yaml:"max_active_runs"`
		QueueTimeoutSeconds    *float64  `yaml:"queue_timeout_seconds"`
		MemoryScope            string    `yaml:"memory_scope"`
		PermissionMode         string    `yaml:"permission_mode"`
		ToolAllowlist          *[]string `yaml:"tool_allowlist"`
		ToolDenylist           []string  `yaml:"tool_denylist"`
		GoalAutoContinue       bool      `yaml:"goal_auto_continue"`
		RunTimeoutSeconds      *float64  `yaml:"run_timeout_seconds"`
		MaxRunTokens           *int64    `yaml:"max_run_tokens"`
		MaxOutputTokens        *int      `yaml:"max_output_tokens"`
		MaxIterations          *int      `yaml:"max_iterations"`
		MaxModelCalls          *int      `yaml:"max_model_calls"`
		MaxToolCalls           *int      `yaml:"max_tool_calls"`
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
		Use            string `yaml:"use"`
		Provider       string `yaml:"provider"`
		AllowHostBash  bool   `yaml:"allow_host_bash"`
		AllowHostTools bool   `yaml:"allow_host_tools"`
	} `yaml:"sandbox"`
	Skills struct {
		Enabled        *bool  `yaml:"enabled"`
		Path           string `yaml:"path"`
		ExtensionsFile string `yaml:"extensions_file"`
		ManagedSources any    `yaml:"managed_sources"`
		ExportRoot     any    `yaml:"export_root"`
		IncludeGlobal  *bool  `yaml:"include_global"`
	} `yaml:"skills"`
	Memory struct {
		Enabled          *bool  `yaml:"enabled"`
		Mode             string `yaml:"mode"`
		InjectionEnabled *bool  `yaml:"injection_enabled"`
		RetrievalEnabled *bool  `yaml:"retrieval_enabled"`
		IncludeGlobal    *bool  `yaml:"include_global"`
		BackendConfig    struct {
			RetrievalEnabled *bool `yaml:"retrieval_enabled"`
		} `yaml:"backend_config"`
	} `yaml:"memory"`
	Summarization struct {
		Enabled   *bool     `yaml:"enabled"`
		ModelName string    `yaml:"model_name"`
		Trigger   yaml.Node `yaml:"trigger"`
		Keep      yaml.Node `yaml:"keep"`
	} `yaml:"summarization"`
}

func modelProvider(use string) (string, error) {
	return harness.NativeModelProvider(use)
}

func (m modelDocument) provider() string {
	if m.Provider != "" {
		return m.Provider
	}
	return m.Use
}

func resolveConfigValue(value string) (string, error) {
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

func configToolNames(values []string) []string {
	return configschema.NormalizeToolNames(values)
}

// ApplyConfig maps model selection and portable daemon settings from
// config.yaml. Explicit Go CLI flags win; unsupported executable/MCP policies
// fail when enabled instead of silently widening Go's authority.
func ApplyConfig(path string, cfg *deerflow.Config, maxConnections *int, explicit map[string]bool) (ConfigSource, error) {
	var result ConfigSource
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
		return result, fmt.Errorf("config exceeds 2 MiB")
	}
	var source runtimeDocument
	if err = yaml.Unmarshal(raw, &source); err != nil {
		return result, fmt.Errorf("parse config: %w", err)
	}
	var native map[string]any
	if err = yaml.Unmarshal(raw, &native); err != nil {
		return result, fmt.Errorf("parse native settings: %w", err)
	}
	if err = configschema.ValidateNative(native); err != nil {
		return result, err
	}
	artifactMiB, err := configschema.ArtifactSizeLimitMiB(native)
	if err != nil {
		return result, err
	}
	cfg.MaxArtifactBytes = artifactMiB * 1024 * 1024
	if cfg.HostMCPRecipes, err = hostMCPRecipes(native); err != nil {
		return result, err
	}
	controls, err := configschema.ProjectSubagentControls(native)
	if err != nil {
		return result, err
	}
	if len(source.ACPAgents) > 0 && !explicit["acp-agents-config"] {
		return result, fmt.Errorf("acp_agents require an explicit --acp-agents-config allowlist")
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
			if source.Sandbox.Provider != "local" && source.Sandbox.Use != "local" && source.Sandbox.Use != "deerflow.sandbox.local:LocalSandboxProvider" {
				return result, fmt.Errorf("Go desktop command mapping requires sandbox.provider=local")
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
		selected := configToolNames(*source.LocalACP.ToolAllowlist)
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
	policy.Denylist = configToolNames(append(slices.Clone(policy.Denylist), source.LocalACP.ToolDenylist...))
	if err := policy.Validate(); err != nil {
		return result, fmt.Errorf("local_acp tool policy: %w", err)
	}
	cfg.ToolPolicy = policy
	if err := applyBuiltinTools(abs, source, cfg); err != nil {
		return result, err
	}
	cfg.GoalAutoContinue = source.LocalACP.GoalAutoContinue
	cfg.WeChat = source.WeChat
	runTimeout := 0.0
	if source.API.ChatRequestTimeout != nil {
		runTimeout = *source.API.ChatRequestTimeout
	}
	if source.LocalACP.RunTimeoutSeconds != nil {
		runTimeout = *source.LocalACP.RunTimeoutSeconds
	}
	if err := configschema.ValidateValue("/runtime/run_timeout_seconds", runTimeout); err != nil {
		return result, err
	}
	queueTimeout := 0.0
	if source.LocalACP.QueueTimeoutSeconds != nil {
		queueTimeout = *source.LocalACP.QueueTimeoutSeconds
	}
	if err := configschema.ValidateValue("/runtime/queue_timeout_seconds", queueTimeout); err != nil {
		return result, err
	}
	// The CLI allow flags are the actual Go MCP authority. A retained Python
	// configuration may set accept_client_mcp_servers without supplying any Go
	// allow flags; the empty policy denies every client MCP transport safely.
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
		return result, fmt.Errorf("config has no selected model")
	}
	var model *modelDocument
	for i := range source.Models {
		if source.Models[i].Name == selected {
			model = &source.Models[i]
			break
		}
	}
	if model == nil || model.Model == "" {
		return result, fmt.Errorf("selected model %q is missing or has no model ID", selected)
	}
	provider, err := modelProvider(model.provider())
	if err != nil {
		return result, err
	}
	apiKey, err := resolveConfigValue(model.APIKey)
	if err != nil {
		return result, err
	}
	baseURL, err := resolveConfigValue(model.BaseURL)
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
	if !explicit["output-token-parameter"] && sameBackend && cfg.Model == model.Model {
		cfg.OutputTokenParameter = model.OutputTokenParameter
	}
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
	if !explicit["run-timeout"] {
		if cfg.Budget == nil {
			budget := harness.DefaultBudgetLimits()
			cfg.Budget = &budget
		}
		cfg.Budget.Timeout = time.Duration(runTimeout * float64(time.Second))
	}
	if source.LocalACP.MaxRunTokens != nil {
		if *source.LocalACP.MaxRunTokens < 0 {
			return result, fmt.Errorf("local_acp.max_run_tokens must not be negative; 0 disables the aggregate token budget")
		}
		if !explicit["max-tokens"] {
			if cfg.Budget == nil {
				budget := harness.DefaultBudgetLimits()
				cfg.Budget = &budget
			}
			cfg.Budget.MaxTokens = *source.LocalACP.MaxRunTokens
		}
	}
	if source.LocalACP.MaxOutputTokens != nil && !explicit["max-output-tokens"] {
		if cfg.Budget == nil {
			budget := harness.DefaultBudgetLimits()
			cfg.Budget = &budget
		}
		cfg.Budget.MaxOutputTokens = *source.LocalACP.MaxOutputTokens
	}
	if source.LocalACP.MaxIterations != nil {
		if *source.LocalACP.MaxIterations < 0 {
			return result, fmt.Errorf("local_acp.max_iterations must not be negative; 0 disables the limit")
		}
		if !explicit["max-iterations"] {
			cfg.MaxIterations = *source.LocalACP.MaxIterations
		}
	}
	for _, limit := range []struct {
		name, flag string
		value      *int
	}{
		{"max_model_calls", "max-model-calls", source.LocalACP.MaxModelCalls},
		{"max_tool_calls", "max-tool-calls", source.LocalACP.MaxToolCalls},
	} {
		if limit.value == nil {
			continue
		}
		if *limit.value < 0 {
			return result, fmt.Errorf("local_acp.%s must not be negative; 0 disables the limit", limit.name)
		}
		if explicit[limit.flag] {
			continue
		}
		if cfg.Budget == nil {
			budget := harness.DefaultBudgetLimits()
			cfg.Budget = &budget
		}
		if limit.flag == "max-model-calls" {
			cfg.Budget.MaxModelCalls = *limit.value
		} else {
			cfg.Budget.MaxToolCalls = *limit.value
		}
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
	if err := configschema.ValidateValue("/runtime/prompt_overlay", overlay); err != nil {
		return result, err
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
	if source.LocalACP.MemoryScope != "" {
		switch source.LocalACP.MemoryScope {
		case string(harness.MemorySession), string(harness.MemoryWorkspace), string(harness.MemoryGlobal):
			cfg.MemoryScope = harness.MemoryScope(source.LocalACP.MemoryScope)
		default:
			return result, fmt.Errorf("local_acp.memory_scope must be global, workspace, or session")
		}
	}
	if source.Memory.Mode != "" {
		if source.Memory.Mode != "middleware" && source.Memory.Mode != "tool" {
			return result, fmt.Errorf("memory.mode must be middleware or tool")
		}
		cfg.MemoryMode = source.Memory.Mode
	}
	if source.Memory.InjectionEnabled != nil {
		cfg.MemoryInjectionEnabled = source.Memory.InjectionEnabled
	}
	if source.Memory.RetrievalEnabled != nil {
		cfg.MemoryRetrievalEnabled = source.Memory.RetrievalEnabled
	}
	if source.Memory.IncludeGlobal != nil {
		cfg.MemoryIncludeGlobal = source.Memory.IncludeGlobal
	}
	if source.Memory.BackendConfig.RetrievalEnabled != nil {
		cfg.MemoryRetrievalEnabled = source.Memory.BackendConfig.RetrievalEnabled
	}
	if err := configschema.ValidateMemoryPolicy(source.LocalACP.MemoryScope, cfg.MemoryMode, cfg.MemoryEnabled == nil || *cfg.MemoryEnabled, cfg.MemoryRetrievalEnabled == nil || *cfg.MemoryRetrievalEnabled, cfg.MemoryIncludeGlobal == nil || *cfg.MemoryIncludeGlobal); err != nil {
		return result, err
	}
	if err := applyConfiguredSummarization(source, cfg, explicit); err != nil {
		return result, err
	}
	if err := applyConfiguredSkills(abs, source, cfg, explicit); err != nil {
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
	// An unoverridden desktop config owns each listed model's route. The ACP
	// choice is only an opaque, public ID; provider endpoints and credentials
	// stay in this host-owned table and are never copied to session metadata.
	configuredRoutes := !explicit["provider"] && !explicit["model"] && !explicit["base-url"] && !explicit["api-key"] && (cfg.APIKey == "" || cfg.APIKey == apiKey)
	modelIDCount := make(map[string]int, len(source.Models))
	modelChoices := make(map[string]string, len(source.Models))
	usedChoices := map[string]bool{cfg.Model: true}
	cfg.ModelRoutes = nil
	cfg.ModelReasoningEfforts = make(map[string][]string)
	cfg.ModelThinking = make(map[string]harness.ThinkingConfig)
	thinkingOutputLimit := harness.DefaultBudgetLimits().MaxOutputTokens
	if cfg.Budget != nil {
		thinkingOutputLimit = cfg.Budget.MaxOutputTokens
	}
	if configuredRoutes {
		cfg.ModelRoutes = make(map[string]harness.ModelRoute)
		seenNames := make(map[string]bool, len(source.Models))
		for _, option := range source.Models {
			if option.Name == "" || seenNames[option.Name] {
				return result, fmt.Errorf("configured model names must be nonempty and unique")
			}
			seenNames[option.Name] = true
			if option.Model != "" {
				modelIDCount[option.Model]++
			}
		}
	}
	for _, option := range source.Models {
		if option.Model == "" {
			continue
		}
		optionProvider, providerErr := modelProvider(option.provider())
		if providerErr != nil {
			continue
		}
		key, keyErr := resolveConfigValue(option.APIKey)
		url, urlErr := resolveConfigValue(option.BaseURL)
		if keyErr != nil || urlErr != nil {
			continue
		}
		matchesDefaultBackend := sameBackend && optionProvider == cfg.Provider && key == cfg.APIKey && url == baseURL
		if !configuredRoutes && !matchesDefaultBackend {
			continue
		}
		choice := option.Model
		if configuredRoutes && option.Name != selected {
			if !matchesDefaultBackend || modelIDCount[option.Model] > 1 || usedChoices[choice] {
				choice = "deerflow-config:" + option.Name
			}
			if usedChoices[choice] {
				return result, fmt.Errorf("configured model choices must be unique")
			}
			usedChoices[choice] = true
			cfg.ModelRoutes[choice] = harness.ModelRoute{Provider: optionProvider, Model: option.Model, BaseURL: url, APIKey: key, OutputTokenParameter: option.OutputTokenParameter}
		}
		if option.Name == selected {
			choice = cfg.Model
		}
		modelChoices[option.Name] = choice
		if err := harness.ValidateReasoningEfforts(optionProvider, option.ReasoningEfforts); err != nil {
			return result, fmt.Errorf("model %q: %w", option.Name, err)
		}
		cfg.ModelReasoningEfforts[choice] = append([]string(nil), option.ReasoningEfforts...)
		thinking := harness.ThinkingConfig{Parameter: option.ThinkingParameter, BudgetTokens: option.ThinkingBudgetTokens}
		if err := thinking.Validate(optionProvider, option.ReasoningEfforts, thinkingOutputLimit); err != nil {
			return result, fmt.Errorf("model %q: %w", option.Name, err)
		}
		cfg.ModelThinking[choice] = thinking
		if option.ContextWindow < 0 || option.ContextWindow > 1<<30 {
			return result, fmt.Errorf("invalid context_window for model %q", option.Name)
		}
		name := option.DisplayName
		if name == "" {
			name = option.Name
		}
		cfg.Models = append(cfg.Models, harness.ConfigValue{Value: choice, Name: name})
		if option.ContextWindow > 0 && !(explicit["context-window"] && choice == cfg.Model) {
			if cfg.ContextWindows == nil {
				cfg.ContextWindows = make(map[string]int)
			}
			cfg.ContextWindows[choice] = option.ContextWindow
		}
		if option.SupportsVision {
			cfg.Media.VisionModels = append(cfg.Media.VisionModels, choice)
		}
	}
	if err := applySubagents(source, cfg, explicit, modelChoices, controls); err != nil {
		return result, err
	}
	var appliedEvolution evolution.ReviewSettings
	if err := applySkillEvolution(abs, source, cfg, modelChoices, &appliedEvolution); err != nil {
		return result, err
	}
	result.Path = abs
	result.Revision = fmt.Sprintf("%x", sha256.Sum256(raw))
	result.Subagents = appliedSubagentSettings(native, controls, cfg, explicit)
	result.Settings = appliedSettings(native, cfg, *maxConnections, explicit, result.Subagents, appliedEvolution)
	return result, nil
}

func applySkillEvolution(configPath string, source runtimeDocument, cfg *deerflow.Config, choices map[string]string, applied *evolution.ReviewSettings) error {
	if source.SkillEvolution.Kind == 0 || source.SkillEvolution.Tag == "!!null" {
		return nil
	}
	data, err := yaml.Marshal(&source.SkillEvolution)
	if err != nil {
		return fmt.Errorf("skill_evolution: %w", err)
	}
	models := make(map[string]bool, len(choices))
	for name := range choices {
		models[name] = true
	}
	defaultModel := source.DefaultModel
	if defaultModel == "" && len(source.Models) > 0 {
		defaultModel = source.Models[0].Name
	}
	settings, err := evolution.DecodeReviewConfig(data, models, defaultModel)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		cfg.SkillProposalSources = nil
		cfg.SkillGeneration = nil
		cfg.SkillLearning, cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator = nil, nil, nil
		cfg.SkillExperienceMatcher = nil
		cfg.SkillDiscovery = nil
		cfg.SkillAutoPatch = nil
		cfg.SkillBehavior, cfg.SkillBehaviorEvaluator = nil, nil
		return nil
	}
	if source.Skills.Enabled != nil && !*source.Skills.Enabled {
		return fmt.Errorf("skill_evolution.enabled requires skills.enabled")
	}
	destinations, err := SkillProposalDestinations(cfg.Skills, settings.AllowBuiltinOverlays)
	if err != nil {
		return err
	}
	if err := ValidateGenerationDestinations(settings.Generation.Enabled, destinations); err != nil {
		return err
	}
	resolve := func(name string) harness.ModelRoute {
		choice := choices[name]
		if route, ok := cfg.ModelRoutes[choice]; ok {
			return route
		}
		return harness.ModelRoute{Provider: cfg.Provider, Model: choice, BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, OutputTokenParameter: cfg.OutputTokenParameter}
	}
	validator, err := deerflow.NewSkillModelValidator(context.Background(), settings.ModelConfig(resolve(settings.ModerationModel), resolve(settings.EvaluationModel)))
	if err != nil {
		return fmt.Errorf("skill_evolution: cannot initialize configured review models")
	}
	cfg.SkillValidator, cfg.SkillProposalSources = validator, destinations
	cfg.SkillGeneration = settings.GenerationOptions(resolve(settings.GenerationModel), destinations)
	cfg.SkillLearning = settings.LearningOptions(resolve(settings.Learning.ExtractionModel))
	if cfg.SkillLearning != nil {
		cfg.SkillLearning.Consolidation = settings.LearningConsolidationModel(resolve(settings.Learning.ConsolidationModel))
		cfg.SkillLearning.Matching = settings.LearningMatchingModel(resolve(settings.Learning.MatchingModel))
	}
	cfg.SkillDiscovery = settings.DiscoveryOptions()
	cfg.SkillAutoPatch = settings.AutoPatchOptions()
	cfg.SkillBehavior, cfg.SkillBehaviorEvaluator = nil, nil
	if settings.Behavior.Enabled {
		options, err := LoadBehaviorSuite(configPath, settings.Behavior.SuitePath, destinations)
		if err != nil {
			return err
		}
		evaluator, err := deerflow.NewSkillBehaviorEvaluator(context.Background(), settings.BehaviorModelConfig(resolve(settings.EvaluationModel)))
		if err != nil {
			return fmt.Errorf("skill_evolution.behavior: cannot initialize configured behavior model")
		}
		cfg.SkillBehavior, cfg.SkillBehaviorEvaluator = options, evaluator
	}
	*applied = settings
	return nil
}

// ValidateGenerationDestinations keeps settings save and SDK startup in agreement.
func ValidateGenerationDestinations(enabled bool, sources []string) error {
	if enabled && (len(sources) == 0 || len(sources) > 32) {
		return fmt.Errorf("skill_evolution.generation: requires 1..32 authorized skill sources")
	}
	return nil
}

// SkillProposalDestinations applies the same source policy at save and launch.
func SkillProposalDestinations(config harness.SkillsConfig, allowBuiltin bool) ([]string, error) {
	var destinations []string
	for _, configured := range config.Sources {
		// Preserve the existing source identity used by installed versions and
		// checkpoints. Renaming it belongs to the explicit source migration.
		if configured.ID == "python-custom" || slices.Contains(config.ManagedSources, configured.ID) {
			destinations = append(destinations, configured.ID)
		}
	}
	if len(destinations) == 0 {
		return nil, fmt.Errorf("skill_evolution.enabled requires an existing custom directory under skills.path or an explicitly configured managed skill source")
	}
	if allowBuiltin {
		granted := false
		for _, id := range config.BuiltinSources {
			destinations = append(destinations, id)
			granted = true
		}
		if !granted {
			return nil, fmt.Errorf("skill_evolution.allow_builtin_overlays requires a configured public skill source")
		}
	}
	return destinations, nil
}

func applySubagents(source runtimeDocument, cfg *deerflow.Config, explicit map[string]bool, choices map[string]string, controls configschema.SubagentControls) error {
	if source.Subagents.Kind != 0 && source.Subagents.Tag != "!!null" {
		data, err := yaml.Marshal(&source.Subagents)
		if err != nil {
			return fmt.Errorf("subagents: %w", err)
		}
		models := make(map[string]bool, len(choices))
		for name := range choices {
			models[name] = true
		}
		settings, err := agents.DecodeConfig(data, models)
		if err != nil {
			return err
		}
		// Configuration names are distinct from provider model IDs and from the
		// opaque ACP choice IDs, especially when two providers reuse a model ID.
		for _, roles := range []map[string]harness.AgentConfig{settings.Agents, settings.CustomAgents} {
			for name, role := range roles {
				if role.Model != "" && role.Model != "inherit" {
					role.Model = choices[role.Model]
				}
				roles[name] = role
			}
		}
		cfg.Subagents = &settings
	}
	if cfg.Subagents == nil {
		cfg.Subagents = &harness.SubagentsConfig{}
	}
	enabled := controls.ResolveEnabled(explicit["disable-subagents"], cfg.DisableSubagents)
	cfg.DisableSubagents = !enabled
	cfg.Subagents.Enabled = &enabled
	concurrent := controls.MaxConcurrent
	cfg.Subagents.MaxConcurrent = &concurrent
	if cfg.Subagents != nil {
		if cfg.Subagents.BackgroundWorkers != nil && !explicit["background-workers"] {
			cfg.BackgroundWorkers = *cfg.Subagents.BackgroundWorkers
		}
		providers := map[string]string{cfg.Model: cfg.Provider}
		for _, model := range cfg.Models {
			providers[model.Value] = cfg.Provider
		}
		for name, route := range cfg.ModelRoutes {
			providers[name] = route.Provider
		}
		limits := harness.DefaultBudgetLimits()
		if cfg.Budget != nil {
			limits = *cfg.Budget
		}
		if err := cfg.Subagents.ValidateModelOptions(providers, limits.MaxOutputTokens); err != nil {
			return err
		}
	}
	return nil
}

func applyBuiltinTools(configPath string, source runtimeDocument, cfg *deerflow.Config) error {
	configured := make([]harness.BuiltinToolConfig, 0, len(source.Tools))
	seen := map[string]bool{}
	for _, setting := range source.Tools {
		if err := setting.Validate(); err != nil {
			return err
		}
		if seen[setting.Name] {
			return fmt.Errorf("duplicate configured tool %s", setting.Name)
		}
		seen[setting.Name] = true
		var err error
		setting.APIKey, err = resolveConfigValue(setting.APIKey)
		if err != nil {
			return fmt.Errorf("%s.api_key: %w", setting.Name, err)
		}
		setting.HTTPSProxy, err = resolveConfigValue(setting.HTTPSProxy)
		if err != nil {
			return fmt.Errorf("%s.https_proxy: %w", setting.Name, err)
		}
		if setting.Executable != "" {
			setting.Executable, err = resolveConfigValue(setting.Executable)
			if err != nil {
				return fmt.Errorf("%s.executable: %w", setting.Name, err)
			}
			if strings.ContainsAny(setting.Executable, "/\\") && !filepath.IsAbs(setting.Executable) {
				setting.Executable = resolveConfigPath(configPath, setting.Executable)
			}
		}
		if err := setting.Validate(); err != nil {
			return err
		}
		configured = append(configured, setting)
	}
	cfg.BuiltinTools = configured
	cfg.HostToolsAllowed = source.Sandbox.AllowHostTools
	return nil
}

type contextSizeDocument struct {
	Type  string  `yaml:"type"`
	Value float64 `yaml:"value"`
}

func decodeContextSizes(node yaml.Node) ([]contextSizeDocument, error) {
	if node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	var values []contextSizeDocument
	if node.Kind == yaml.SequenceNode {
		if err := node.Decode(&values); err != nil {
			return nil, err
		}
	} else if node.Kind == yaml.MappingNode {
		var value contextSizeDocument
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		values = []contextSizeDocument{value}
	} else {
		return nil, fmt.Errorf("context size must be an object or list")
	}
	return values, nil
}

func applyConfiguredSummarization(source runtimeDocument, cfg *deerflow.Config, explicit map[string]bool) error {
	setting := source.Summarization
	if setting.Enabled != nil && !*setting.Enabled {
		if !explicit["context-compaction"] {
			cfg.Compaction.Enabled = false
		}
		return nil
	}
	if strings.TrimSpace(setting.ModelName) != "" {
		return fmt.Errorf("summarization.model_name requires an explicit Go compaction model adapter")
	}
	if !explicit["context-compaction"] {
		cfg.Compaction.Enabled = true
	}
	defaultThresholds := setting.Trigger.Kind == 0 || setting.Trigger.Tag == "!!null"
	if defaultThresholds {
		// Omitted thresholds use the same defaults as the CLI. A keep-only
		// setting still needs decoding and validation below.
		setting.Trigger = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		if err := setting.Trigger.Encode([]map[string]any{}); err != nil {
			return err
		}
	}
	values, err := decodeContextSizes(setting.Trigger)
	if err != nil {
		return fmt.Errorf("summarization.trigger: %w", err)
	}
	if len(values) == 0 && !defaultThresholds || len(values) > 8 {
		return fmt.Errorf("summarization.trigger must contain 1..8 supported thresholds")
	}
	// Go's Eino middleware accepts both message and token thresholds. Use its
	// supported maxima for an omitted dimension so a single Python trigger does
	// not acquire an earlier, unrelated threshold.
	messages, tokens := 10_000, 2_000_000
	if defaultThresholds {
		// Resolve automatic tokens per role's selected model during assembly;
		// no default message threshold should compact a large model early.
		messages, tokens = 0, 0
	}
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
	if defaultThresholds {
		keep = 8
	}
	if setting.Keep.Kind != 0 && setting.Keep.Tag != "!!null" {
		values, err = decodeContextSizes(setting.Keep)
		if err != nil || len(values) != 1 || values[0].Type != "messages" || math.IsNaN(values[0].Value) || math.IsInf(values[0].Value, 0) || values[0].Value != math.Trunc(values[0].Value) || values[0].Value < 1 || values[0].Value > 1000 {
			return fmt.Errorf("summarization.keep must be 1..1000 messages")
		}
		keep = int(values[0].Value)
	}
	if messages > 0 && messages <= keep+1 {
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

func resolveConfigPath(configPath, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(configPath), value))
}

func configuredSkillStates(configPath string, source runtimeDocument) (map[string]bool, error) {
	name := source.API.ExtensionsConfigPath
	if name == "" {
		name = source.Skills.ExtensionsFile
	}
	if name == "" {
		name = "./extensions_config.json"
	}
	file, err := os.Open(resolveConfigPath(configPath, name))
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

func configuredSkillName(path string) string {
	name, _ := inspectSkillDocument(path)
	return name
}

func inspectSkillDocument(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot read SKILL.md")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return "", fmt.Errorf("SKILL.md is unreadable or exceeds 1 MiB")
	}
	name, _, err := skills.ParseSkillDescription(data)
	return name, err
}

func applyConfiguredSkills(configPath string, source runtimeDocument, cfg *deerflow.Config, explicit map[string]bool) error {
	if explicit["skills-config"] {
		return nil
	}
	if source.Skills.Enabled != nil && !*source.Skills.Enabled {
		cfg.Skills = harness.SkillsConfig{}
		cfg.SkillSelection = harness.SkillSelection{Names: []string{}}
		return nil
	}
	managed, err := skills.ResolveManagedSources(configPath, source.Skills.ManagedSources, source.Skills.ExportRoot)
	if err != nil {
		return err
	}
	if source.Skills.Path == "" {
		if source.Skills.Enabled != nil && *source.Skills.Enabled && len(managed.Sources) == 0 {
			return fmt.Errorf("skills.enabled requires skills.path or skills.managed_sources")
		}
		if len(managed.Sources) > 0 {
			cfg.Skills = managed
			cfg.SkillSelection = harness.SkillSelection{IncludeGlobal: source.Skills.IncludeGlobal != nil && *source.Skills.IncludeGlobal}
		}
		return nil
	}
	root := resolveConfigPath(configPath, source.Skills.Path)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skills.path must name an existing ordinary directory")
	}
	states, err := configuredSkillStates(configPath, source)
	if err != nil {
		return err
	}
	config := managed
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
		if len(config.Sources) > 32 {
			return fmt.Errorf("skills: at most 32 combined import and managed sources are supported")
		}
		if category == "public" {
			config.BuiltinSources = append(config.BuiltinSources, id)
		}
		entries := 0
		err = filepath.WalkDir(categoryRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() && (entry.Name() == ".deerflow-edit-stage" || entry.Name() == ".deerflow-edit-backups" || entry.Name() == ".deerflow-publication-backups" || entry.Name() == ".deerflow-publication-stage" || entry.Name() == ".deerflow-exports") {
				return filepath.SkipDir
			}
			entries++
			if entries > 4096 {
				return fmt.Errorf("skills %s exceeds 4096 directory entries", category)
			}
			if entry.IsDir() || entry.Name() != "SKILL.md" || !entry.Type().IsRegular() {
				return nil
			}
			dir := filepath.Dir(path)
			name, parseErr := inspectSkillDocument(path)
			if parseErr != nil || filepath.Base(dir) != name {
				message := "skill name must match its installation directory"
				if parseErr != nil {
					message = parseErr.Error()
				}
				if len(config.Diagnostics) < 128 {
					config.Diagnostics = append(config.Diagnostics, harness.SkillFinding{Code: "invalid_skill_document", Path: path, Message: message})
				}
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
	if source.Skills.IncludeGlobal != nil {
		cfg.SkillSelection.IncludeGlobal = *source.Skills.IncludeGlobal
	}
	return nil
}
