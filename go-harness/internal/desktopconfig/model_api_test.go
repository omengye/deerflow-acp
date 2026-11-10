package desktopconfig

import (
	"bytes"
	"os"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestModelAPISettingsRoundTrip(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	first := modelByName(t, doc, "first")
	if first["api"] != "auto" {
		t.Fatal("legacy models did not retain automatic protocol")
	}
	first["api"] = "responses"
	doc = run(t, options, "save", doc)
	if modelByName(t, doc, "first")["api"] != "responses" {
		t.Fatal("saved Responses selection was not returned")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.API != harness.ModelAPIResponses {
		t.Fatal("saved protocol did not reach the default model")
	}
	delete(modelByName(t, doc, "first"), "api")
	doc = run(t, options, "save", doc)
	if modelByName(t, doc, "first")["api"] != "responses" {
		t.Fatal("older client erased saved protocol")
	}
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range []map[string]any{
		{"api": "invalid"},
		{"api": 1},
		{"api": "responses", "provider": "claude"},
		{"api": "responses", "output_token_parameter": "max_completion_tokens"},
		{"api": "responses", "output_token_parameter": "max_tokens"},
		{"output_token_parameter": "max_tokens"},
	} {
		candidate := run(t, options, "snapshot", nil)
		model := modelByName(t, candidate, "first")
		if _, present := update["api"]; !present {
			delete(model, "api") // A legacy draft must validate the retained API.
		}
		for key, value := range update {
			model[key] = value
		}
		if _, err := Run(options, "save", mustJSON(t, candidate)); err == nil {
			t.Fatalf("accepted unsupported protocol options: %v", update)
		}
	}
	after, err := os.ReadFile(options.Config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected protocol update modified saved configuration")
	}
}
