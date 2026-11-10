package harness

import "fmt"

// ModelOutputLimits is host-owned configuration for one model route. The
// requested allowance includes reasoning where the provider counts it as
// output. A zero allowance inherits the host limit; a zero supported limit
// means unknown, never an assertion that the service supports unlimited output.
type ModelOutputLimits struct {
	MaxOutputTokens          int `json:"maxOutputTokens,omitempty"`
	MaxSupportedOutputTokens int `json:"maxSupportedOutputTokens,omitempty"`
}

func (l ModelOutputLimits) Validate(hostLimit int) error {
	if l.MaxOutputTokens < 0 || l.MaxOutputTokens > 1<<30 || l.MaxSupportedOutputTokens < 0 || l.MaxSupportedOutputTokens > 1<<30 {
		return fmt.Errorf("model output token limits must be between 0 and %d", 1<<30)
	}
	if l.MaxSupportedOutputTokens > 0 && l.MaxOutputTokens > l.MaxSupportedOutputTokens {
		return fmt.Errorf("max_output_tokens (%d) exceeds this model service's max_supported_output_tokens (%d)", l.MaxOutputTokens, l.MaxSupportedOutputTokens)
	}
	if hostLimit > 0 && l.MaxOutputTokens > hostLimit {
		return fmt.Errorf("model max_output_tokens (%d) exceeds the host max_output_tokens hard limit (%d)", l.MaxOutputTokens, hostLimit)
	}
	return nil
}

// Effective only tightens positive limits. Callers further constrain this by
// role, request, context and shared run budgets before provider admission.
func (l ModelOutputLimits) Effective(hostLimit int) int {
	limit := hostLimit
	for _, candidate := range []int{l.MaxOutputTokens, l.MaxSupportedOutputTokens} {
		if candidate > 0 && (limit == 0 || candidate < limit) {
			limit = candidate
		}
	}
	return limit
}
