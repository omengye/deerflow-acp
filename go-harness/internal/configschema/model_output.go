package configschema

import (
	"encoding/json"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func modelOutputLimits(model map[string]any) (harness.ModelOutputLimits, error) {
	var limits harness.ModelOutputLimits
	for key, target := range map[string]*int{
		"max_output_tokens":           &limits.MaxOutputTokens,
		"max_supported_output_tokens": &limits.MaxSupportedOutputTokens,
	} {
		value := model[key]
		if value == nil {
			continue
		}
		if err := validate(numeric("models/*", key, "integer", 0, 0, 1<<30), value); err != nil {
			return limits, err
		}
		// After schema validation these are bounded, integral numbers, whether
		// decoded from native YAML or from a desktop JSON draft.
		encoded, err := json.Marshal(value)
		if err != nil {
			return limits, err
		}
		var number float64
		if err := json.Unmarshal(encoded, &number); err != nil {
			return limits, err
		}
		*target = int(number)
	}
	return limits, nil
}

// ValidateModelOutputLimits validates a complete save candidate, including its
// host hard limit. Launch also validates after applying explicit CLI overrides.
func ValidateModelOutputLimits(document map[string]any) error {
	host := harness.DefaultBudgetLimits().MaxOutputTokens
	if local, ok := document["local_acp"].(map[string]any); ok && local["max_output_tokens"] != nil {
		limits, err := modelOutputLimits(local)
		if err != nil {
			return err
		}
		host = limits.MaxOutputTokens
	}
	models, _ := document["models"].([]any)
	for index, value := range models {
		model, _ := value.(map[string]any)
		limits, err := modelOutputLimits(model)
		if err == nil {
			err = limits.Validate(host)
		}
		if err != nil {
			return fmt.Errorf("models.%d: %w", index, err)
		}
	}
	return nil
}
