package desktopconfig

import (
	"os"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestModelOutputSettingsRoundTripAndRejectInvalidSave(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	first := modelByName(t, doc, "first")
	first["max_output_tokens"], first["max_supported_output_tokens"] = 16384, 64000
	doc["runtime"].(map[string]any)["max_output_tokens"] = 16384
	saved := run(t, options, "save", doc)
	var cfg deerflow.Config
	if _, err := launch.ApplyConfig(options.Config, &cfg, new(int), nil); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ModelOutputLimits[cfg.Model]; got.MaxOutputTokens != 16384 || got.MaxSupportedOutputTokens != 64000 {
		t.Fatalf("limits not loaded: %+v", got)
	}
	delete(modelByName(t, saved, "first"), "max_output_tokens")
	delete(modelByName(t, saved, "first"), "max_supported_output_tokens")
	saved = run(t, options, "save", saved)
	if got := modelByName(t, saved, "first"); !configschema.SameValue(got["max_output_tokens"], 16384) || !configschema.SameValue(got["max_supported_output_tokens"], 64000) {
		t.Fatalf("old draft erased limits: output=%v service=%v", got["max_output_tokens"], got["max_supported_output_tokens"])
	}
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ output, supported, host any }{
		{16384, 8192, 16384}, {16384, 64000, 4096}, {-1, 64000, 16384}, {1.5, 64000, 16384}, {"16384", 64000, 16384}, {nil, 64000, 16384}, {4096, -1, 16384},
	} {
		first := modelByName(t, saved, "first")
		first["max_output_tokens"], first["max_supported_output_tokens"] = tc.output, tc.supported
		saved["runtime"].(map[string]any)["max_output_tokens"] = tc.host
		for _, operation := range []string{"validate", "save"} {
			if _, err := Run(options, operation, mustJSON(t, saved)); err == nil {
				t.Fatalf("%s accepted invalid limits: %+v", operation, tc)
			}
		}
	}
	after, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected save modified config")
	}
}
