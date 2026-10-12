package harness

import "testing"

func TestResolveOutputTokenParameter(t *testing.T) {
	for _, tc := range []struct {
		name, provider, endpoint string
		configured, want         OutputTokenParameter
		invalid                  bool
	}{
		{"official", "openai", "https://api.openai.com/v1", "", OutputTokensCompletion, false},
		{"default", "", "", OutputTokensAuto, OutputTokensCompletion, false},
		{"unknown", "openai-compatible", "https://gateway.example/v1", "", OutputTokensCompletion, false},
		{"tencent", "openai", "https://copilot.tencent.com/v2", "", OutputTokensLegacy, false},
		{"tencent-case", "OPENAI", "https://COPILOT.TENCENT.COM:443/v2/", OutputTokensAuto, OutputTokensLegacy, false},
		{"lookalike", "openai", "https://copilot.tencent.com.evil.test/v2", "", OutputTokensCompletion, false},
		{"userinfo", "openai", "https://copilot.tencent.com@other.test/v2", "", OutputTokensCompletion, false},
		{"path", "openai", "https://other.test/copilot.tencent.com/v2", "", OutputTokensCompletion, false},
		{"override-new", "openai", "https://copilot.tencent.com/v2", OutputTokensCompletion, OutputTokensCompletion, false},
		{"override-old", "openai", "https://proxy.example/v1", OutputTokensLegacy, OutputTokensLegacy, false},
		{"claude", "claude", "", "", OutputTokensLegacy, false},
		{"anthropic", "anthropic", "", OutputTokensLegacy, OutputTokensLegacy, false},
		{"claude-invalid", "claude", "", OutputTokensCompletion, "", true},
		{"unsupported-responses", "openai", "", "max_output_tokens", "", true},
		{"invalid-provider", "other", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveOutputTokenParameter(tc.provider, tc.endpoint, tc.configured)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("got=%q err=%v want=%q invalid=%t", got, err, tc.want, tc.invalid)
			}
		})
	}
}
