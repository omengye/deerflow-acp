package harness

import (
	"fmt"
	"net/url"
	"strings"
)

// OutputTokenParameter selects the wire field, never a second output budget.
type OutputTokenParameter string

const (
	OutputTokensAuto       OutputTokenParameter = "auto"
	OutputTokensLegacy     OutputTokenParameter = "max_tokens"
	OutputTokensCompletion OutputTokenParameter = "max_completion_tokens"
)

// ResolveOutputTokenParameter is independent of reasoning effort. Unknown
// OpenAI-compatible services use the modern field unless explicitly overridden;
// accepting an HTTP request is not evidence that a gateway enforces its cap.
func ResolveOutputTokenParameter(provider, baseURL string, configured OutputTokenParameter) (OutputTokenParameter, error) {
	if configured != "" && configured != OutputTokensAuto && configured != OutputTokensLegacy && configured != OutputTokensCompletion {
		return "", fmt.Errorf("output_token_parameter must be auto, max_tokens, or max_completion_tokens")
	}
	provider = strings.ToLower(provider)
	switch provider {
	case "claude", "anthropic":
		if configured == OutputTokensCompletion {
			return "", fmt.Errorf("Claude Messages requires output_token_parameter=max_tokens or auto")
		}
		return OutputTokensLegacy, nil
	case "", "openai", "openai-compatible":
		if configured != "" && configured != OutputTokensAuto {
			return configured, nil
		}
		endpoint, err := url.Parse(baseURL)
		if err == nil && strings.EqualFold(endpoint.Hostname(), "copilot.tencent.com") {
			return OutputTokensLegacy, nil
		}
		return OutputTokensCompletion, nil
	default:
		return "", fmt.Errorf("output token policy is unsupported by provider %q", provider)
	}
}
