package launch

import (
	"os"
	"path/filepath"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestApplyOutputTokenPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `default_model: first
local_acp:
  max_output_tokens: 16384
models:
  - name: first
    provider: openai
    model: first-id
    base_url: https://proxy.example/v1
    output_token_parameter: max_tokens
  - name: second
    provider: openai
    model: second-id
    base_url: https://other.example/v1
    output_token_parameter: max_completion_tokens
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 0
	source, err := ApplyConfig(path, &cfg, &connections, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutputTokenParameter != harness.OutputTokensLegacy || cfg.ModelRoutes["deerflow-config:second"].OutputTokenParameter != harness.OutputTokensCompletion || cfg.Budget.MaxOutputTokens != 16384 {
		t.Fatalf("output policy not loaded: default=%s routes=%v budget=%+v", cfg.OutputTokenParameter, cfg.ModelRoutes, cfg.Budget)
	}
	if field := source.Settings.Fields["/runtime/max_output_tokens"]; field.Value != 16384 || field.Source != "yaml" {
		t.Fatalf("applied=%+v", field)
	}
	cfg.OutputTokenParameter, cfg.Budget.MaxOutputTokens = harness.OutputTokensCompletion, 512
	source, err = ApplyConfig(path, &cfg, &connections, map[string]bool{"output-token-parameter": true, "max-output-tokens": true})
	if err != nil || cfg.OutputTokenParameter != harness.OutputTokensCompletion || cfg.Budget.MaxOutputTokens != 512 || source.Settings.Fields["/runtime/max_output_tokens"].Source != "cli" {
		t.Fatalf("CLI overrides lost: parameter=%s budget=%+v applied=%+v err=%v", cfg.OutputTokenParameter, cfg.Budget, source.Settings.Fields["/runtime/max_output_tokens"], err)
	}
}

func TestOutputTokenPolicyDoesNotCrossBackendOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("models:\n  - name: first\n    provider: openai\n    model: first-id\n    base_url: https://copilot.tencent.com/v2\n    output_token_parameter: max_tokens\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := deerflow.Config{BaseURL: "https://api.openai.com/v1"}
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, map[string]bool{"base-url": true}); err != nil {
		t.Fatal(err)
	}
	if cfg.OutputTokenParameter != "" {
		t.Fatalf("carried model policy to another backend: %s", cfg.OutputTokenParameter)
	}
}
