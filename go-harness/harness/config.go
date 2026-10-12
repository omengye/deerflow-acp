package harness

import (
	"fmt"
	"slices"
)

// Model reasoning capabilities are explicitly configured, never inferred from
// a model name or an OpenAI-compatible endpoint.
func ValidateReasoningEfforts(provider string, efforts []string) error {
	seen := make(map[string]bool, len(efforts))
	for _, effort := range efforts {
		if seen[effort] || effort == "" {
			return fmt.Errorf("reasoning_efforts contains an empty or duplicate value")
		}
		if err := (AgentConfig{ReasoningEffort: effort}).ValidateModelOptions(provider, 0); err != nil {
			return err
		}
		seen[effort] = true
	}
	return nil
}

func ReasoningEffortAllowed(efforts []string, effort string) bool {
	return effort == "" || effort == "default" || slices.Contains(efforts, effort)
}

const (
	ApprovalAsk          = "ask"
	ApprovalAllowAlways  = "allow_always"
	ApprovalRejectAlways = "reject_always"
	ApprovalReadOnly     = "read_only"
)

// ConfigValue describes an allowlisted choice without exposing provider secrets.
type ConfigValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ModelRoute is host-only configuration for an allowed session model choice.
// APIKey must never be included in ACP config options or persisted sessions.
type ModelRoute struct {
	Provider             string
	Model                string
	BaseURL              string
	APIKey               string
	OutputTokenParameter OutputTokenParameter `json:",omitempty"`
}

// ConfigOption is transport-independent metadata for a select control. Values
// only become available when the runtime actually consumes them.
type ConfigOption struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	Category     string         `json:"category,omitempty"`
	Type         string         `json:"type"`
	CurrentValue string         `json:"currentValue"`
	Options      []ConfigValue  `json:"options"`
	Meta         map[string]any `json:"_meta,omitempty"`
}
