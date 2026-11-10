package harness

import "testing"

func TestModelAPICompatibility(t *testing.T) {
	for _, provider := range []string{"", "openai", "openai-compatible"} {
		for _, configured := range []ModelAPI{"", ModelAPIAuto, ModelAPIChatCompletions} {
			api, err := ResolveModelAPI(provider, configured)
			if err != nil || api != ModelAPIChatCompletions {
				t.Fatalf("legacy protocol changed: provider=%q api=%q err=%v", provider, api, err)
			}
			if canonical, err := CanonicalModelAPI(provider, configured); err != nil || canonical != "" {
				t.Fatal("legacy protocol changed the runtime identity")
			}
		}
		api, err := ResolveModelAPI(provider, ModelAPIResponses)
		if err != nil || api != ModelAPIResponses {
			t.Fatalf("Responses unavailable: provider=%q api=%q err=%v", provider, api, err)
		}
	}
	for _, provider := range []string{"claude", "anthropic"} {
		if api, err := ResolveModelAPI(provider, ""); err != nil || api != ModelAPIAuto {
			t.Fatalf("Claude default changed: api=%q err=%v", api, err)
		}
		for _, configured := range []ModelAPI{ModelAPIResponses, ModelAPIChatCompletions} {
			if _, err := ResolveModelAPI(provider, configured); err == nil {
				t.Fatalf("accepted unsupported Claude protocol: %q", configured)
			}
		}
	}
	if _, err := ResolveModelAPI("openai", "other"); err == nil {
		t.Fatal("accepted unknown model protocol")
	}
	for _, output := range []OutputTokenParameter{OutputTokensLegacy, OutputTokensCompletion} {
		if err := ValidateModelAPIOptions("openai", ModelAPIResponses, output); err == nil {
			t.Fatalf("Responses silently ignored output parameter: %q", output)
		}
	}
}
