package launch

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/opencli"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

// AppliedSettings projects only explicitly selected non-secret values from the
// configuration actually loaded by this daemon. It never marshals a Config,
// model route, tool configuration, MCP environment or credential.
type AppliedSettings struct {
	Version   string                    `json:"version"`
	AppliesTo string                    `json:"applies_to"`
	Fields    map[string]AppliedSetting `json:"fields"`
	// Collections stay separate from the <=96 scalar fields understood by
	// older clients. Only configured role overrides are listed, not guessed
	// per-session inheritance or grants. The launch validator bounds both lists.
	Roles []AppliedSettingsGroup `json:"roles,omitempty"`
	Tools []AppliedSettingsGroup `json:"tools,omitempty"`
}

type AppliedSettingsGroup struct {
	Name   string                    `json:"name"`
	Kind   string                    `json:"kind,omitempty"`
	Fields map[string]AppliedSetting `json:"fields"`
}

type AppliedSetting struct {
	Value      any    `json:"value"`
	Source     string `json:"source"`
	ConfigPath string `json:"configPath,omitempty"`
}

func appliedSettings(document map[string]any, cfg *deerflow.Config, maxConnections int, explicit map[string]bool, roles AppliedSubagentSettings, review evolution.ReviewSettings) AppliedSettings {
	out := AppliedSettings{Version: "deerflow-applied-settings-v1", AppliesTo: "new_sessions_and_tasks", Fields: map[string]AppliedSetting{}}
	has := func(path string) bool {
		var value any = document
		for _, part := range strings.Split(path, ".") {
			object, ok := value.(map[string]any)
			if !ok {
				return false
			}
			value, ok = object[part]
			if !ok || value == nil {
				return false
			}
		}
		return true
	}
	add := func(path string, value any, flag string, keys ...string) {
		setting := AppliedSetting{Value: value, Source: "default"}
		for _, key := range keys {
			if has(key) {
				setting.Source, setting.ConfigPath = "yaml", key
				break
			}
		}
		if flag != "" && explicit[flag] {
			setting.Source = "cli"
		}
		out.Fields[path] = setting
	}
	on := func(value *bool) bool { return value == nil || *value }
	add("/runtime/model_name", cfg.Model, "model", "local_acp.model_name", "api.model_name", "default_model", "models")
	add("/runtime/provider", cfg.Provider, "provider", "models")
	add("/runtime/max_active_connections", maxConnections, "max-connections", "local_acp.max_active_connections")
	add("/runtime/max_active_runs", cfg.MaxActiveRuns, "max-active-runs", "local_acp.max_active_runs")
	add("/runtime/queue_timeout_seconds", cfg.QueueTimeout.Seconds(), "queue-timeout", "local_acp.queue_timeout_seconds")
	if cfg.Budget != nil {
		add("/runtime/run_timeout_seconds", cfg.Budget.Timeout.Seconds(), "run-timeout", "local_acp.run_timeout_seconds", "api.chat_request_timeout")
	}
	add("/runtime/permission_mode", cfg.PermissionMode, "", "local_acp.permission_mode")
	add("/runtime/tool_allowlist", cfg.ToolPolicy.Allowlist, "", "local_acp.tool_allowlist")
	add("/runtime/tool_denylist", cfg.ToolPolicy.Denylist, "", "local_acp.tool_denylist")
	add("/runtime/session_cleanup_enabled", cfg.Retention.Enabled, "session-cleanup-enabled", "local_acp.session_cleanup_enabled")
	add("/runtime/inactive_session_retention_days", cfg.Retention.InactiveDays, "inactive-session-retention-days", "local_acp.inactive_session_retention_days")
	add("/runtime/closed_session_retention_days", cfg.Retention.ClosedDays, "closed-session-retention-days", "local_acp.closed_session_retention_days")
	add("/runtime/session_cleanup_interval_seconds", cfg.Retention.CheckInterval.Seconds(), "session-cleanup-interval", "local_acp.session_cleanup_interval_seconds")
	memoryOverride := ""
	if cfg.MemoryExtraction && explicit["memory-extraction"] {
		memoryOverride = "memory-extraction"
	}
	add("/memory/enabled", on(cfg.MemoryEnabled), memoryOverride, "memory.enabled")
	// Extraction is a separate effective gate and may be explicitly disabled
	// while reading/injection remain enabled.
	add("/memory/extraction_enabled", cfg.MemoryExtraction, "memory-extraction", "memory.enabled")
	add("/memory/mode", cfg.MemoryMode, "", "memory.mode")
	add("/memory/injection_enabled", on(cfg.MemoryEnabled) && cfg.MemoryMode != "tool" && on(cfg.MemoryInjectionEnabled), "", "memory.mode", "memory.injection_enabled", "memory.enabled")
	add("/memory/retrieval_enabled", on(cfg.MemoryEnabled) && on(cfg.MemoryRetrievalEnabled), "", "memory.backend_config.retrieval_enabled", "memory.retrieval_enabled", "memory.enabled")
	add("/runtime/memory_scope", cfg.MemoryScope, "", "local_acp.memory_scope")
	add("/memory/include_global", on(cfg.MemoryEnabled) && on(cfg.MemoryIncludeGlobal), "", "memory.include_global", "memory.enabled")
	add("/sandbox/provider", cfg.Sandbox.Provider, "sandbox-provider", "sandbox.provider", "sandbox.use")
	add("/sandbox/enabled", cfg.Sandbox.Enabled, "sandbox-provider", "local_acp.enable_bash")
	add("/sandbox/allow_shell", cfg.Sandbox.AllowShell, "sandbox-allow-shell", "local_acp.enable_bash", "sandbox.allow_host_bash")
	add("/sandbox/allow_host_tools", cfg.HostToolsAllowed, "", "sandbox.allow_host_tools")
	add("/skills/include_global", cfg.SkillSelection.IncludeGlobal, "skills-config", "skills.include_global")
	add("/skills/source_count", len(cfg.Skills.Sources), "skills-config", "skills.managed_sources", "skills.path")
	add("/skills/managed_source_count", len(cfg.Skills.ManagedSources), "skills-config", "skills.managed_sources")
	add("/skills/model_submission_enabled", len(cfg.SkillProposalSources) > 0, "", "skill_evolution.enabled")
	add("/skill_evolution/generation/enabled", cfg.SkillGeneration != nil, "", "skill_evolution.generation.enabled")
	add("/skill_evolution/discovery/enabled", cfg.SkillDiscovery != nil, "", "skill_evolution.discovery")
	add("/skill_evolution/behavior/enabled", cfg.SkillBehavior != nil, "", "skill_evolution.behavior.enabled")
	mode := "review"
	if cfg.SkillAutoPatch != nil {
		mode = "auto_patch"
	}
	add("/skill_evolution/mode", mode, "", "skill_evolution.mode")
	// Read worker options already accepted by launch. ReviewSettings is used
	// only for numeric model limits hidden behind the validator interfaces.
	evolutionValue := func(key string, value any) {
		add("/skill_evolution/"+key, value, "", "skill_evolution."+strings.ReplaceAll(key, "/", "."))
	}
	if review.Enabled {
		evolutionValue("validation/timeout_seconds", review.Validation.TimeoutSeconds)
		evolutionValue("validation/max_input_bytes", review.Validation.MaxInputBytes)
		evolutionValue("validation/max_output_tokens", review.Validation.MaxOutputTokens)
	}
	if generation := cfg.SkillGeneration; generation != nil {
		evolutionValue("generation/timeout_seconds", appliedDefault(review.Generation.TimeoutSeconds, 60))
		evolutionValue("generation/max_input_bytes", appliedDefault(generation.Model.MaxInputBytes, 131072))
		evolutionValue("generation/max_output_tokens", appliedDefault(generation.Model.MaxOutputTokens, 8192))
		evolutionValue("budget/max_daily_model_calls", generation.Budget.MaxDailyModelCalls)
		evolutionValue("budget/max_daily_tokens", generation.Budget.MaxDailyTokens)
		evolutionValue("budget/max_job_seconds", generation.Budget.MaxJobSeconds)
		evolutionValue("budget/workers", generation.Budget.Workers)
	}
	if discovery := cfg.SkillDiscovery; discovery != nil {
		evolutionValue("discovery/operator_corrections_enabled", !discovery.DisableOperatorCorrections)
		evolutionValue("discovery/missing_reports_enabled", discovery.MissingReportsEnabled)
		evolutionValue("discovery/successful_workflows_enabled", discovery.SuccessfulWorkflowsEnabled)
		evolutionValue("discovery/new_skill_source_id", discovery.NewSkillSourceID)
		evolutionValue("discovery/repeat_threshold", discovery.RepeatThreshold)
		evolutionValue("discovery/repeat_window_days", discovery.RepeatWindowDays)
		evolutionValue("discovery/cooldown_hours", discovery.CooldownHours)
		evolutionValue("discovery/max_daily_proposals", discovery.MaxDailyProposals)
		evolutionValue("discovery/max_pending_proposals", discovery.MaxPendingProposals)
	}
	if cfg.SkillBehavior != nil {
		evolutionValue("behavior/timeout_seconds", review.Behavior.TimeoutSeconds)
		evolutionValue("behavior/max_steps", review.Behavior.MaxSteps)
		evolutionValue("behavior/max_input_bytes", review.Behavior.MaxInputBytes)
		evolutionValue("behavior/max_output_tokens", review.Behavior.MaxOutputTokens)
	}
	if patch := cfg.SkillAutoPatch; patch != nil {
		evolutionValue("auto_patch/max_changed_lines", patch.MaxChangedLines)
		evolutionValue("auto_patch/max_daily_publications", patch.MaxDailyPublications)
		evolutionValue("auto_patch/observation_hours", patch.ObservationHours)
		evolutionValue("auto_patch/min_observations", patch.MinObservations)
		evolutionValue("auto_patch/rollback_failure_threshold", patch.RollbackFailureThreshold)
	}
	add("/mcp/allowed_command_count", len(cfg.MCP.AllowedCommands), "mcp-allow-command")
	add("/mcp/allow_http", cfg.MCP.AllowHTTP, "mcp-allow-http")
	add("/mcp/allow_sse", cfg.MCP.AllowSSE, "mcp-allow-sse")
	add("/subagents/background_workers", cfg.BackgroundWorkers, "background-workers", "subagents.background_workers")
	out.Fields["/subagents/enabled"] = AppliedSetting{Value: roles.Enabled, Source: roles.EnabledSource}
	out.Fields["/subagents/max_concurrent"] = AppliedSetting{Value: roles.MaxConcurrent, Source: roles.ConcurrencySource}
	// Instructions may contain private deployment details. Expose only a
	// digest of the loaded text, enough to distinguish a saved-but-unapplied edit.
	add("/runtime/instruction_sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(cfg.Instruction))), "", "local_acp.prompt_overlay_file", "local_acp.prompt_overlay")
	out.Roles = appliedRoleOverrides(cfg.Subagents)
	out.Tools = appliedToolParameters(cfg)
	return out
}

func appliedDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func appliedRoleOverrides(config *harness.SubagentsConfig) []AppliedSettingsGroup {
	if config == nil {
		return nil
	}
	var groups []AppliedSettingsGroup
	for _, collection := range []struct {
		name  string
		roles map[string]harness.AgentConfig
	}{{"agents", config.Agents}, {"custom_agents", config.CustomAgents}} {
		names := make([]string, 0, len(collection.roles))
		for name := range collection.roles {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if len(groups) >= 64 {
				return groups
			}
			role := collection.roles[name]
			group := AppliedSettingsGroup{Name: name, Kind: collection.name, Fields: map[string]AppliedSetting{}}
			put := func(key string, value any) {
				group.Fields[key] = AppliedSetting{Value: value, Source: "yaml", ConfigPath: "subagents." + collection.name + "." + name + "." + strings.ReplaceAll(key, "/", ".")}
			}
			// Prompts and descriptions can contain private instructions. Only
			// digests identify loaded revisions; no arbitrary text is projected.
			if role.SystemPrompt != "" {
				put("system_prompt_sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(role.SystemPrompt))))
			}
			if role.Description != "" {
				put("description_sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(role.Description))))
			}
			if role.Model != "" {
				put("model", role.Model)
			}
			if role.ReasoningEffort != "" {
				put("reasoning_effort", role.ReasoningEffort)
			}
			if role.ThinkingEnabled != nil {
				put("thinking_enabled", *role.ThinkingEnabled)
			}
			if role.TimeoutSeconds != nil {
				put("timeout_seconds", *role.TimeoutSeconds)
			}
			if role.MaxTurns != nil {
				put("max_turns", *role.MaxTurns)
			}
			if role.ModelSettings.Temperature != nil {
				put("model_settings/temperature", *role.ModelSettings.Temperature)
			}
			if role.ModelSettings.MaxTokens != nil {
				put("model_settings/max_tokens", *role.ModelSettings.MaxTokens)
			}
			if role.ModelSettings.ThinkingBudgetTokens != nil {
				put("model_settings/thinking_budget_tokens", *role.ModelSettings.ThinkingBudgetTokens)
			}
			for _, list := range []struct {
				key    string
				values []string
			}{
				{"tools", role.Tools}, {"disallowed_tools", role.DisallowedTools}, {"skills", role.Skills}, {"delegate_to", role.DelegateTo},
			} {
				if list.values != nil {
					put(list.key, slices.Clone(list.values))
				}
			}
			groups = append(groups, group)
		}
	}
	return groups
}

func appliedToolParameters(cfg *deerflow.Config) []AppliedSettingsGroup {
	var groups []AppliedSettingsGroup
	for index, tool := range cfg.BuiltinTools {
		if len(groups) >= 13 {
			break
		}
		group := AppliedSettingsGroup{Name: tool.Name, Fields: map[string]AppliedSetting{}}
		put := func(key string, value any, configured bool) {
			source := "default"
			if configured {
				source = "yaml"
			}
			group.Fields[key] = AppliedSetting{Value: value, Source: source, ConfigPath: fmt.Sprintf("tools.%d.%s", index, key)}
		}
		// This describes the loaded host parameters. Tool policy, session mode
		// and role grants may still prevent any particular invocation.
		switch tool.Name {
		case "web_search", "image_search":
			put("timeout", appliedDefault(tool.Timeout, 30), tool.Timeout != 0)
			put("max_results", appliedDefault(tool.MaxResults, 5), tool.MaxResults != 0)
		case "web_fetch":
			put("timeout", appliedDefault(tool.Timeout, 10), tool.Timeout != 0)
			put("max_output_chars", appliedDefault(tool.MaxOutputChars, 4096), tool.MaxOutputChars != 0)
		case "glob":
			put("max_results", appliedDefault(tool.MaxResults, 200), tool.MaxResults != 0)
		case "grep":
			put("max_results", appliedDefault(tool.MaxResults, 100), tool.MaxResults != 0)
		case "host_opencli":
			put("timeout", appliedDefault(tool.Timeout, 120), tool.Timeout != 0)
			put("allowed_sites", slices.Clone(opencli.AllowedSites(tool)), tool.AllowedSites != nil)
			// The launcher may resolve .cmd to a native executable. Identify the
			// configured entry without claiming it is the eventual process argv.
			if tool.Executable != "" {
				put("executable_sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(tool.Executable))), true)
			}
		}
		// No API key, proxy URL/userinfo, environment or launch arguments.
		if len(group.Fields) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}
