package desktopconfig

import (
	"os"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestOutputTokenSettingsRoundTrip(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	first := modelByName(t, doc, "first")
	if first["output_token_parameter"] != "auto" {
		t.Fatalf("missing auto policy: %v", first["output_token_parameter"])
	}
	first["output_token_parameter"] = "max_tokens"
	doc["runtime"].(map[string]any)["max_output_tokens"] = 16384
	saved := run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.OutputTokenParameter != harness.OutputTokensLegacy || cfg.Budget.MaxOutputTokens != 16384 {
		t.Fatalf("saved policy not loaded: %s %+v", cfg.OutputTokenParameter, cfg.Budget)
	}
	// An older desktop can omit the new model capability on an unrelated save.
	delete(modelByName(t, saved, "first"), "output_token_parameter")
	saved = run(t, options, "save", saved)
	if modelByName(t, saved, "first")["output_token_parameter"] != "max_tokens" {
		t.Fatal("older draft erased policy")
	}
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, parameter := range []any{"max_output_tokens", "invalid", 128, nil} {
		modelByName(t, saved, "first")["output_token_parameter"] = parameter
		if _, err := Run(options, "save", mustJSON(t, saved)); err == nil {
			t.Fatalf("accepted invalid policy %v", parameter)
		}
	}
	modelByName(t, saved, "first")["provider"] = "claude"
	modelByName(t, saved, "first")["output_token_parameter"] = "max_completion_tokens"
	if _, err := Run(options, "save", mustJSON(t, saved)); err == nil {
		t.Fatal("accepted unsupported Claude policy")
	}
	after, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("invalid save modified configuration")
	}
}
