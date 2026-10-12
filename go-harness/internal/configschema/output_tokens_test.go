package configschema

import "testing"

func TestOutputTokenFields(t *testing.T) {
	for _, value := range []any{"auto", "max_tokens", "max_completion_tokens"} {
		if err := ValidateValue("/models/*/output_token_parameter", value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []any{-1, 1.5, "auto", float64(1<<30) + 1} {
		if err := ValidateValue("/runtime/max_output_tokens", value); err == nil {
			t.Fatalf("accepted output budget %v", value)
		}
	}
	for _, value := range []any{0, 4096, 16384} {
		if err := ValidateValue("/runtime/max_output_tokens", value); err != nil {
			t.Fatal(err)
		}
	}
	for _, provider := range []string{"claude", "anthropic"} {
		doc := map[string]any{"models": []any{map[string]any{"use": provider, "model": "fixture", "output_token_parameter": "max_completion_tokens"}}}
		if err := ValidateNative(doc); err == nil {
			t.Fatalf("accepted invalid native provider policy: %s", provider)
		}
	}
}
