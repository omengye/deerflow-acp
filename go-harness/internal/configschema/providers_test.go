package configschema

import (
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestModelProvidersUseOnlyOpenAIAndClaude(t *testing.T) {
	for _, value := range []string{"openai", "claude"} {
		if err := ValidateValue("/models/*/provider", value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"ark", "unknown"} {
		if err := ValidateValue("/models/*/provider", value); err == nil {
			t.Fatalf("provider remains selectable: %s", value)
		}
		if _, err := harness.NativeModelProvider(value); err == nil {
			t.Fatalf("provider remains routable: %s", value)
		}
		doc := map[string]any{"models": []any{map[string]any{"provider": value, "model": "fixture"}}}
		if err := ValidateNative(doc); err == nil {
			t.Fatalf("provider remains configurable: %s", value)
		}
	}
}
