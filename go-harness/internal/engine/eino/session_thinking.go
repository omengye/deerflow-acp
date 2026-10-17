package eino

import (
	"fmt"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

func (e *Engine) sessionTuning(selectedModel string, session harness.Session) (*roleModelTuning, error) {
	config := e.config.ModelThinking[selectedModel]
	if err := config.ValidateSelection(session.ThinkingMode, session.ReasoningEffort); err != nil {
		return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
	}
	efforts := e.config.ModelReasoningEfforts[selectedModel]
	effort := session.ReasoningEffort
	if !harness.ReasoningEffortAllowed(efforts, effort) {
		return nil, fmt.Errorf("%w: reasoning effort is not configured for this model", harness.ErrInvalidInput)
	}
	if effort == "default" {
		effort = ""
	}
	if session.ThinkingMode == "" || session.ThinkingMode == "default" {
		return e.roleTuning(agents.Spec{Model: selectedModel, ReasoningEffort: effort})
	}
	enabled := session.ThinkingMode == "on"
	if config.Parameter == "claude" {
		spec := agents.Spec{Model: selectedModel, ThinkingEnabled: &enabled}
		if enabled {
			budget := config.EnabledBudget()
			spec.ModelSettings.ThinkingBudgetTokens = &budget
		}
		return e.roleTuning(spec)
	}
	provider := e.config.Provider
	if route, ok := e.config.ModelRoutes[selectedModel]; ok {
		provider = route.Provider
	}
	tuning := &roleModelTuning{provider: strings.ToLower(provider), effort: effort}
	switch config.Parameter {
	case "enable_thinking":
		tuning.extraFields = map[string]any{"enable_thinking": enabled}
	case "thinking_type":
		value := "disabled"
		if enabled {
			value = "enabled"
		}
		tuning.extraFields = map[string]any{"thinking": map[string]any{"type": value}}
	case "reasoning_effort":
		if !enabled {
			tuning.effort = "none"
		}
	}
	return tuning, nil
}
