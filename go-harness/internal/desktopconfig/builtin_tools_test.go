package desktopconfig

import (
	"testing"
)

func TestNativeDesktopConfigSaveKeepsSecretsAndRemovesPythonUses(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	doc["tool_groups"] = []any{map[string]any{"name": "web"}, map[string]any{"name": "host:opencli"}}
	doc["tools"] = []any{map[string]any{"name": "web_search", "group": "web", "api_key": "fixture-tool-secret", "https_proxy": "http://127.0.0.1:11808", "max_results": 5}, map[string]any{"name": "host_opencli", "group": "host:opencli", "executable": "opencli"}}
	run(t, options, "save", doc)
	saved, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list(saved, "models") {
		model := item.(map[string]any)
		if model["use"] != nil || str(model, "provider", "") != "openai" || str(model, "api_key", "") == "" {
			t.Fatal("native model migration lost credential/provider")
		}
	}
	if readObject(saved, "sandbox")["use"] != nil || str(readObject(saved, "sandbox"), "provider", "") != "local" {
		t.Fatal("native sandbox migration missing")
	}
	snapshot := run(t, options, "snapshot", nil)
	tool := list(snapshot, "tools")[0].(map[string]any)
	if str(tool, "api_key", "") == "fixture-tool-secret" {
		t.Fatal("snapshot exposed tool key")
	}
	run(t, options, "save", snapshot)
	saved, err = readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if str(list(saved, "tools")[0].(map[string]any), "api_key", "") != "fixture-tool-secret" {
		t.Fatal("redacted tool credential was lost on save")
	}
}
