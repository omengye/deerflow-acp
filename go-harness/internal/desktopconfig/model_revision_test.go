package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewModelsSaveWithCurrentRevisionsAfterValidationAndInit(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	extensions := filepath.Join(filepath.Dir(options.Config), "extensions_config.json")
	extensionBytes, err := os.ReadFile(extensions)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"new-model", "another-new-model"} {
		doc["models"] = append(list(doc, "models"), map[string]any{
			"original_name": "", "name": name, "display_name": "", "description": "",
			"provider": "openai", "model": name, "base_url": "https://example.test/v1",
			"api_key": "new-model-key", "api_key_configured": false, "api_key_literal": false,
			"clear_api_key": false, "supports_vision": false, "context_window": 0,
			"output_token_parameter": "auto",
		})
		configRevision, extensionsRevision := doc["config_revision"], doc["extensions_revision"]
		run(t, options, "init", nil)
		run(t, options, "validate", doc)
		current := run(t, options, "snapshot", nil)
		if current["config_revision"] != configRevision || current["extensions_revision"] != extensionsRevision {
			t.Fatal("initialization or validation invalidated the model draft")
		}
		doc = run(t, options, "save", doc)
		if modelByName(t, doc, name)["original_name"] != name {
			t.Fatal("new model did not round trip as a saved model")
		}
		if doc["config_revision"] == configRevision {
			t.Fatal("new model save did not refresh the configuration revision")
		}
		if doc["extensions_revision"] != extensionsRevision {
			t.Fatal("model save changed the extensions revision")
		}
	}
	currentExtensions, err := os.ReadFile(extensions)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(currentExtensions, extensionBytes) {
		t.Fatal("model configuration saves changed extension contents")
	}
}

func TestRebasedNewModelPreservesExternalToolRepairAndHiddenKeyRotation(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	raw["tools"] = append(list(raw, "tools"), map[string]any{
		"name": "host_opencli", "executable": "old-opencli.cmd",
	})
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	base := run(t, options, "snapshot", nil)
	base["models"] = append(list(base, "models"), map[string]any{
		"original_name": "", "name": "new-model", "display_name": "", "description": "",
		"provider": "openai", "model": "new-model-id", "base_url": "https://new.test/v1",
		"api_key": "new-model-private-key", "api_key_configured": false, "api_key_literal": false,
		"clear_api_key": false, "supports_vision": false, "context_window": 0,
		"output_token_parameter": "auto",
	})
	toolByName(t, raw, "host_opencli")["executable"] = "repaired-opencli.cmd"
	modelByName(t, raw, "first")["api_key"] = "externally-rotated-private-key"
	raw["external_only_setting"] = map[string]any{"preserve": true}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"validate", "save"} {
		if _, err := Run(options, operation, mustJSON(t, base)); err == nil || !strings.Contains(err.Error(), "配置已变化") {
			t.Fatalf("%s accepted a stale model draft: %v", operation, err)
		}
	}
	after, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected stale model draft changed external configuration")
	}
	latest := run(t, options, "snapshot", nil)
	if modelByName(t, latest, "first")["api_key"] != "" {
		t.Fatal("latest snapshot exposed the rotated literal credential")
	}
	// The desktop merge keeps latest metadata and tools while carrying forward
	// the stale draft's unchanged existing models and newly configured model.
	latest["models"] = base["models"]
	run(t, options, "validate", latest)
	saved := run(t, options, "save", latest)
	if toolByName(t, saved, "host_opencli")["executable"] != "repaired-opencli.cmd" {
		t.Fatal("rebased model save reverted the external tool repair")
	}
	persisted, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if modelByName(t, persisted, "first")["api_key"] != "externally-rotated-private-key" {
		t.Fatal("rebased model save reverted the hidden credential rotation")
	}
	if modelByName(t, persisted, "new-model")["api_key"] != "new-model-private-key" {
		t.Fatal("rebased model save lost the new model credential")
	}
	if readObject(persisted, "external_only_setting")["preserve"] != true {
		t.Fatal("rebased model save removed an externally configured unknown field")
	}
}
