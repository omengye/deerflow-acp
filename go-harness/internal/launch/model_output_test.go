package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
)

func TestModelOutputLimitsLoadAndValidate(t *testing.T) {
	for _, tc := range []struct {
		name, fields, wantError string
	}{
		{"configured", "max_output_tokens: 16384\n    max_supported_output_tokens: 64000", ""},
		{"unknown", "max_output_tokens: 0\n    max_supported_output_tokens: 0", ""},
		{"service ceiling", "max_output_tokens: 16384\n    max_supported_output_tokens: 8192", "max_supported_output_tokens"},
		{"host ceiling", "max_output_tokens: 32768\n    max_supported_output_tokens: 64000", "host"},
		{"negative", "max_output_tokens: -1", "max_output_tokens"},
		{"fractional", "max_output_tokens: 1.5", "max_output_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			raw := fmt.Sprintf("default_model: first\nlocal_acp:\n  max_output_tokens: 16384\nmodels:\n  - name: first\n    provider: openai\n    model: first-id\n    max_output_tokens: 4096\n  - name: second\n    provider: openai\n    model: second-id\n    base_url: https://other.example/v1\n    %s\n", tc.fields)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			var cfg deerflow.Config
			_, err := ApplyConfig(path, &cfg, new(int), nil)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("expected %s, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ModelOutputLimits["first-id"].MaxOutputTokens != 4096 {
				t.Fatal("default model allowance lost")
			}
			other := cfg.ModelOutputLimits["deerflow-config:second"]
			if tc.name == "configured" && (other.MaxOutputTokens != 16384 || other.MaxSupportedOutputTokens != 64000) {
				t.Fatalf("route limits lost: %+v", other)
			}
			if tc.name == "unknown" && other.MaxSupportedOutputTokens != 0 {
				t.Fatal("unknown service limit was guessed")
			}
			// A different CLI endpoint must not inherit the original model's caps.
			override := deerflow.Config{BaseURL: "https://override.example/v1"}
			if _, err := ApplyConfig(path, &override, new(int), map[string]bool{"base-url": true}); err != nil {
				t.Fatal(err)
			}
			if len(override.ModelOutputLimits) != 0 {
				t.Fatalf("policy crossed backend override: %+v", override.ModelOutputLimits)
			}
		})
	}
}
