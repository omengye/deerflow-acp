package harness

import "testing"

func TestThinkingConfigurationValidation(t *testing.T) {
	for _, tc := range []struct {
		parameter, provider string
		budget, cap         int
		efforts             []string
		valid               bool
	}{
		{"", "openai", 0, 4096, nil, true},
		{"unsupported", "openai", 0, 4096, []string{"high"}, true},
		{"unsupported", "claude", 0, 4096, nil, true},
		{"enable_thinking", "openai", 0, 4096, nil, true},
		{"thinking_type", "openai-compatible", 0, 4096, nil, true},
		{"reasoning_effort", "openai", 0, 4096, []string{"high"}, true},
		{"reasoning_effort", "openai", 0, 4096, nil, false},
		{"enable_thinking", "claude", 0, 4096, nil, false},
		{"claude", "openai", 0, 4096, nil, false},
		{"claude", "claude", 0, 4096, nil, true},
		{"claude", "anthropic", 2048, 4096, nil, true},
		{"claude", "claude", 100, 4096, nil, false},
		{"claude", "claude", 4096, 4096, nil, false},
		{"enable_thinking", "openai", 1024, 4096, nil, false},
		{"unknown", "openai", 0, 4096, nil, false},
	} {
		if err := (ThinkingConfig{Parameter: tc.parameter, BudgetTokens: tc.budget}).Validate(tc.provider, tc.efforts, tc.cap); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestThinkingSelectionDoesNotInferDefaultEffort(t *testing.T) {
	for _, parameter := range []string{"enable_thinking", "thinking_type", "reasoning_effort", "claude", "", "unsupported"} {
		config := ThinkingConfig{Parameter: parameter}
		for _, mode := range []string{"", "default", "on", "off"} {
			for _, effort := range []string{"", "default", "high"} {
				want := ThinkingModeAllowed(config, mode) && !(parameter == "reasoning_effort" && mode == "on" && effort != "high")
				if err := config.ValidateSelection(mode, effort); (err == nil) != want {
					t.Errorf("%s %s %s: %v", parameter, mode, effort, err)
				}
			}
		}
	}
	if (ThinkingConfig{}).Availability() != "unconfigured" || (ThinkingConfig{Parameter: "unsupported"}).Availability() != "unsupported" {
		t.Fatal("unknown and unsupported capabilities conflated")
	}
}
