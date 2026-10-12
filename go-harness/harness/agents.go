package harness

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
)

// SubagentsConfig is host-owned deployment configuration. Model-visible tool
// arguments can select a registered role, but cannot supply a new AgentConfig.
// Pointer fields preserve explicit false/zero and nil versus empty selections.
type SubagentsConfig struct {
	Enabled              *bool                  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	TimeoutSeconds       *float64               `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	MaxTurns             *int                   `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	MaxDepth             *int                   `json:"max_depth,omitempty" yaml:"max_depth,omitempty"`
	MaxConcurrent        *int                   `json:"max_concurrent,omitempty" yaml:"max_concurrent,omitempty"`
	BackgroundWorkers    *int                   `json:"background_workers,omitempty" yaml:"background_workers,omitempty"`
	MaxTasksPerRoot      *int                   `json:"max_tasks_per_root,omitempty" yaml:"max_tasks_per_root,omitempty"`
	AutoContinue         *bool                  `json:"auto_continue,omitempty" yaml:"auto_continue,omitempty"`
	MaxAutoContinuations *int                   `json:"max_auto_continuations,omitempty" yaml:"max_auto_continuations,omitempty"`
	Agents               map[string]AgentConfig `json:"agents,omitempty" yaml:"agents,omitempty"`
	CustomAgents         map[string]AgentConfig `json:"custom_agents,omitempty" yaml:"custom_agents,omitempty"`
}

type AgentModelSettings struct {
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	// ThinkingBudgetTokens applies to Claude's explicit thinking mode. A nil
	// value uses 1024; it is always contained within the granted output cap.
	ThinkingBudgetTokens *int `json:"thinking_budget_tokens,omitempty" yaml:"thinking_budget_tokens,omitempty"`
}

type AgentConfig struct {
	Description     string             `json:"description,omitempty" yaml:"description,omitempty"`
	SystemPrompt    string             `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	Model           string             `json:"model,omitempty" yaml:"model,omitempty"`
	Tools           []string           `json:"tools" yaml:"tools"`
	DisallowedTools []string           `json:"disallowed_tools" yaml:"disallowed_tools"`
	Skills          []string           `json:"skills" yaml:"skills"`
	TimeoutSeconds  *float64           `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	MaxTurns        *int               `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	ModelSettings   AgentModelSettings `json:"model_settings" yaml:"model_settings"`
	ThinkingEnabled *bool              `json:"thinking_enabled,omitempty" yaml:"thinking_enabled,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
	DelegateTo      []string           `json:"delegate_to" yaml:"delegate_to"`
}

var agentNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ValidAgentName is shared by configuration field and role graph validation.
func ValidAgentName(name string) bool { return agentNamePattern.MatchString(name) }

// Validate checks syntax and references without mutating caller-owned config.
// A nil models map skips existence checks; deployment callers pass their actual
// model catalog. The inheritance keyword never names a credential or backend.
func (c SubagentsConfig) Validate(models map[string]bool) error {
	for _, v := range []struct {
		path     string
		value    *int
		min, max int
	}{
		{"max_turns", c.MaxTurns, 0, 10000},
		{"max_depth", c.MaxDepth, 1, 3},
		{"max_concurrent", c.MaxConcurrent, 1, 64},
		{"background_workers", c.BackgroundWorkers, 1, 64},
		{"max_tasks_per_root", c.MaxTasksPerRoot, 1, 10000},
		{"max_auto_continuations", c.MaxAutoContinuations, 0, 1000},
	} {
		if v.value != nil && (*v.value < v.min || *v.value > v.max) {
			return fmt.Errorf("subagents.%s must be %d..%d", v.path, v.min, v.max)
		}
	}
	if err := validateAgentTimeout("subagents.timeout_seconds", c.TimeoutSeconds); err != nil {
		return err
	}
	if len(c.Agents)+len(c.CustomAgents) > 64 {
		return fmt.Errorf("subagents: at most 64 configured roles are allowed")
	}
	roles := map[string]bool{"general-purpose": true, "bash": true}
	for name := range c.CustomAgents {
		if !ValidAgentName(name) || roles[name] {
			return fmt.Errorf("subagents.custom_agents.%s: invalid or reserved role name", name)
		}
		roles[name] = true
	}
	for _, collection := range []struct {
		name   string
		roles  map[string]AgentConfig
		custom bool
	}{{"agents", c.Agents, false}, {"custom_agents", c.CustomAgents, true}} {
		names := make([]string, 0, len(collection.roles))
		for name := range collection.roles {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			agent := collection.roles[name]
			path := "subagents." + collection.name + "." + name
			if !collection.custom && name != "general-purpose" && name != "bash" {
				return fmt.Errorf("%s: unknown builtin role; define custom roles in custom_agents", path)
			}
			if collection.custom && (strings.TrimSpace(agent.Description) == "" || strings.TrimSpace(agent.SystemPrompt) == "") {
				return fmt.Errorf("%s: description and system_prompt are required", path)
			}
			if len(agent.Description) > 4096 || len(agent.SystemPrompt) > 65536 || strings.ContainsRune(agent.SystemPrompt, 0) {
				return fmt.Errorf("%s: invalid or oversized role instructions", path)
			}
			if agent.Model != strings.TrimSpace(agent.Model) || len(agent.Model) > 256 || strings.ContainsAny(agent.Model, "\x00\r\n") {
				return fmt.Errorf("%s.model: invalid model reference", path)
			}
			if agent.Model != "" && agent.Model != "inherit" && models != nil && !models[agent.Model] {
				return fmt.Errorf("%s.model: unknown model %q", path, agent.Model)
			}
			if err := (ToolPolicy{Allowlist: agent.Tools, Denylist: agent.DisallowedTools}).Validate(); err != nil {
				return fmt.Errorf("%s.tools: %w", path, err)
			}
			for _, list := range []struct {
				name   string
				values []string
			}{{"skills", agent.Skills}, {"delegate_to", agent.DelegateTo}} {
				if len(list.values) > 128 {
					return fmt.Errorf("%s.%s: too many names", path, list.name)
				}
				seen := map[string]bool{}
				for _, value := range list.values {
					if !ValidAgentName(value) || seen[value] {
						return fmt.Errorf("%s.%s: invalid or duplicate name %q", path, list.name, value)
					}
					seen[value] = true
					if list.name == "delegate_to" && !roles[value] {
						return fmt.Errorf("%s.delegate_to: unknown role %q", path, value)
					}
				}
			}
			if err := validateAgentTimeout(path+".timeout_seconds", agent.TimeoutSeconds); err != nil {
				return err
			}
			if p := agent.MaxTurns; p != nil && (*p < 0 || *p > 10000) {
				return fmt.Errorf("%s.max_turns must be 0..10000; 0 disables the limit", path)
			}
			if p := agent.ModelSettings.Temperature; p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > 2) {
				return fmt.Errorf("%s.model_settings.temperature must be 0..2", path)
			}
			if p := agent.ModelSettings.MaxTokens; p != nil && (*p < 1 || *p > 1<<30) {
				return fmt.Errorf("%s.model_settings.max_tokens must be 1..2^30", path)
			}
			if p := agent.ModelSettings.ThinkingBudgetTokens; p != nil && (*p < 1024 || *p >= 1<<30) {
				return fmt.Errorf("%s.model_settings.thinking_budget_tokens must be 1024..2^30-1", path)
			}
			if agent.ReasoningEffort != "" && !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, agent.ReasoningEffort) {
				return fmt.Errorf("%s.reasoning_effort is invalid", path)
			}
		}
	}
	// Reject cycles even when max_depth would eventually stop execution: a
	// deployment mistake should be visible before spending a model budget.
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("subagents: delegation cycle at role %q", name)
		}
		if done[name] {
			return nil
		}
		visiting[name] = true
		a, ok := c.CustomAgents[name]
		if !ok {
			a = c.Agents[name]
		}
		for _, child := range a.DelegateTo {
			if err := visit(child); err != nil {
				return err
			}
		}
		visiting[name] = false
		done[name] = true
		return nil
	}
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func validateAgentTimeout(path string, value *float64) error {
	if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value != 0 && (*value < .001 || *value > MaxExecutionTimeoutSeconds)) {
		return fmt.Errorf("%s must be 0 (unlimited) or 0.001..%d", path, MaxExecutionTimeoutSeconds)
	}
	return nil
}
