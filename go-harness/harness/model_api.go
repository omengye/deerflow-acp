package harness

import (
	"fmt"
	"strings"
)

// ModelAPI selects the request protocol independently of the provider and model
// name. Auto preserves the provider's existing protocol.
type ModelAPI string

const (
	ModelAPIAuto            ModelAPI = "auto"
	ModelAPIChatCompletions ModelAPI = "chat_completions"
	ModelAPIResponses       ModelAPI = "responses"
)

func ResolveModelAPI(provider string, configured ModelAPI) (ModelAPI, error) {
	if configured == "" {
		configured = ModelAPIAuto
	}
	switch strings.ToLower(provider) {
	case "", "openai", "openai-compatible":
		switch configured {
		case ModelAPIAuto, ModelAPIChatCompletions:
			return ModelAPIChatCompletions, nil
		case ModelAPIResponses:
			return ModelAPIResponses, nil
		}
	case "claude", "anthropic":
		if configured == ModelAPIAuto {
			return ModelAPIAuto, nil
		}
	}
	return "", fmt.Errorf("api must use auto, or chat_completions/responses for an OpenAI-compatible provider")
}

func ValidateModelAPIOptions(provider string, configured ModelAPI, output OutputTokenParameter) error {
	api, err := ResolveModelAPI(provider, configured)
	if err != nil {
		return err
	}
	if api == ModelAPIResponses && output != "" && output != OutputTokensAuto {
		return fmt.Errorf("Responses requires output_token_parameter auto; its output limit uses max_output_tokens")
	}
	return nil
}

// CanonicalModelAPI keeps the legacy default absent from host route identities.
// Selecting auto or explicit Chat Completions must not invalidate existing runs.
func CanonicalModelAPI(provider string, configured ModelAPI) (ModelAPI, error) {
	api, err := ResolveModelAPI(provider, configured)
	if err != nil {
		return "", err
	}
	if api == ModelAPIResponses {
		return api, nil
	}
	return "", nil
}
