package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestConfiguredRequestLimits(t *testing.T) {
	for _, tc := range []struct {
		name, settings            string
		flags                     []string
		iterations, models, tools int
		wantError                 string
	}{
		{name: "defaults", iterations: harness.DefaultMaxIterations},
		{name: "yaml", settings: "  max_iterations: 250\n  max_model_calls: 300\n  max_tool_calls: 600\n", iterations: 250, models: 300, tools: 600},
		{name: "yaml-disables-call-limits", settings: "  max_model_calls: 0\n  max_tool_calls: 0\n"},
		{name: "cli-wins", settings: "  max_iterations: 250\n  max_model_calls: 300\n  max_tool_calls: 600\n", flags: []string{"--max-iterations=80", "--max-model-calls=90", "--max-tool-calls=120"}, iterations: 80, models: 90, tools: 120},
		{name: "cli-disables-yaml-limits", settings: "  max_iterations: 250\n  max_model_calls: 300\n  max_tool_calls: 600\n", flags: []string{"--max-iterations=0", "--max-model-calls=0", "--max-tool-calls=0"}},
		{name: "zero-iterations", settings: "  max_iterations: 0\n"},
		{name: "negative-iterations", settings: "  max_iterations: -1\n", wantError: "max_iterations"},
		{name: "negative-model-calls", settings: "  max_model_calls: -1\n", wantError: "max_model_calls"},
		{name: "negative-tool-calls", settings: "  max_tool_calls: -1\n", wantError: "max_tool_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseRuntimeFlags(t, tc.flags...)
			if err != nil {
				t.Fatal(err)
			}
			explicit := map[string]bool{}
			for _, arg := range tc.flags {
				name, _, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
				explicit[name] = true
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			raw := "local_acp:\n" + tc.settings + "models:\n  - name: fixture\n    provider: openai\n    model: fixture\n"
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			connections := 0
			_, err = ApplyConfig(path, &cfg, &connections, explicit)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxIterations != tc.iterations || cfg.Budget.MaxModelCalls != tc.models || cfg.Budget.MaxToolCalls != tc.tools {
				t.Fatalf("iterations=%d budget=%+v", cfg.MaxIterations, cfg.Budget)
			}
		})
	}
}
