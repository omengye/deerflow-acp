// Package agents resolves host-owned roles without importing Eino or ACP types.
package agents

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Spec is an immutable execution recipe. It contains model references, never
// credentials. Resolve returns independent copies for each execution boundary.
type Spec struct {
	Name            string                     `json:"name"`
	Description     string                     `json:"description"`
	Instruction     string                     `json:"instruction"`
	Model           string                     `json:"model"`
	Tools           harness.ToolPolicy         `json:"tools"`
	Skills          []string                   `json:"skills"`
	MaxTurns        int                        `json:"maxTurns"`
	TimeoutSeconds  float64                    `json:"timeoutSeconds"`
	ModelSettings   harness.AgentModelSettings `json:"modelSettings"`
	ThinkingEnabled *bool                      `json:"thinkingEnabled,omitempty"`
	ReasoningEffort string                     `json:"reasoningEffort,omitempty"`
	DelegateTo      []string                   `json:"delegateTo"`
	Version         string                     `json:"version,omitempty"`
}

type Registry struct {
	config harness.SubagentsConfig
	roles  map[string]Spec
}

type Parent struct {
	Model          string
	Tools          harness.ToolPolicy
	Skills         []string
	MaxTurns       int
	TimeoutSeconds float64
}

func New(config harness.SubagentsConfig, models map[string]bool) (*Registry, error) {
	if err := config.Validate(models); err != nil {
		return nil, err
	}
	// JSON preserves explicit empty lists, and detaches pointers/maps from the
	// caller so later config edits cannot modify in-flight role recipes.
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	var pinned harness.SubagentsConfig
	if err = json.Unmarshal(data, &pinned); err != nil {
		return nil, err
	}
	r := &Registry{config: pinned, roles: map[string]Spec{}}
	defaults := map[string]harness.AgentConfig{
		"general-purpose": {Description: "Complete a delegated task with an isolated conversation.", SystemPrompt: "Complete the delegated task and return results, evidence and unresolved issues to the parent."},
		"bash":            {Description: "Execute a bounded command task using the configured command tool.", SystemPrompt: "Complete the delegated command task. Report the command outcome and relevant output.", Tools: []string{"bash"}},
	}
	for name, base := range defaults {
		override, ok := pinned.Agents[name]
		if ok {
			if override.Description == "" {
				override.Description = base.Description
			}
			if override.SystemPrompt == "" {
				override.SystemPrompt = base.SystemPrompt
			}
			if override.Tools == nil {
				override.Tools = base.Tools
			}
			base = override
		}
		r.roles[name] = r.spec(name, base)
	}
	for name, c := range pinned.CustomAgents {
		r.roles[name] = r.spec(name, c)
	}
	return r, nil
}

func (r *Registry) spec(name string, c harness.AgentConfig) Spec {
	turns, timeout := 50, 900.0
	if r.config.MaxTurns != nil {
		turns = *r.config.MaxTurns
	}
	if r.config.TimeoutSeconds != nil {
		timeout = *r.config.TimeoutSeconds
	}
	if c.MaxTurns != nil {
		turns = *c.MaxTurns
	}
	if c.TimeoutSeconds != nil {
		timeout = *c.TimeoutSeconds
	}
	return Spec{Name: name, Description: c.Description, Instruction: c.SystemPrompt, Model: c.Model,
		Tools: harness.ToolPolicy{Allowlist: c.Tools, Denylist: c.DisallowedTools}, Skills: c.Skills,
		MaxTurns: turns, TimeoutSeconds: timeout, ModelSettings: c.ModelSettings, ThinkingEnabled: c.ThinkingEnabled, ReasoningEffort: c.ReasoningEffort, DelegateTo: c.DelegateTo}
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.roles))
	for name := range r.roles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (r *Registry) Resolve(name string, parent Parent) (Spec, error) {
	base, ok := r.roles[name]
	if !ok {
		return Spec{}, fmt.Errorf("%w: unknown subagent %q", harness.ErrInvalidInput, name)
	}
	data, err := json.Marshal(base)
	if err != nil {
		return Spec{}, err
	}
	var spec Spec
	if err = json.Unmarshal(data, &spec); err != nil {
		return Spec{}, err
	}
	if spec.Model == "" || spec.Model == "inherit" {
		spec.Model = parent.Model
	}
	spec.Tools.Allowlist = intersection(parent.Tools.Allowlist, spec.Tools.Allowlist)
	spec.Tools.Denylist = append(slices.Clone(parent.Tools.Denylist), spec.Tools.Denylist...)
	slices.Sort(spec.Tools.Denylist)
	spec.Tools.Denylist = slices.Compact(spec.Tools.Denylist)
	spec.Skills = intersection(parent.Skills, spec.Skills)
	if parent.MaxTurns > 0 && parent.MaxTurns < spec.MaxTurns {
		spec.MaxTurns = parent.MaxTurns
	}
	if parent.TimeoutSeconds > 0 && parent.TimeoutSeconds < spec.TimeoutSeconds {
		spec.TimeoutSeconds = parent.TimeoutSeconds
	}
	// Canonical selections give equivalent policies the same version.
	slices.Sort(spec.Tools.Allowlist)
	slices.Sort(spec.Skills)
	slices.Sort(spec.DelegateTo)
	data, err = json.Marshal(spec)
	if err != nil {
		return Spec{}, err
	}
	spec.Version = fmt.Sprintf("%x", sha256.Sum256(data))
	return spec, nil
}

// Validate verifies a persisted recipe's digest. This is integrity evidence;
// execution still requires validation of the current host authorization.
func (s Spec) Validate() error {
	version := s.Version
	s.Version = ""
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if version == "" || version != fmt.Sprintf("%x", sha256.Sum256(data)) || s.Name == "" || s.Description == "" || s.Instruction == "" || s.MaxTurns < 1 || s.TimeoutSeconds <= 0 {
		return fmt.Errorf("%w: invalid immutable role recipe", harness.ErrInvalidInput)
	}
	return s.Tools.Validate()
}

// nil means inherited/unrestricted, while [] means no capability.
func intersection(parent, child []string) []string {
	if parent == nil {
		return slices.Clone(child)
	}
	if child == nil {
		return slices.Clone(parent)
	}
	result := make([]string, 0, len(child))
	for _, name := range child {
		if slices.Contains(parent, name) {
			result = append(result, name)
		}
	}
	return result
}
