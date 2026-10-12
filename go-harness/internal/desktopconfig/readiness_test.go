package desktopconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadinessChecksSavedConfigWithoutSecretsOrMutations(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	result := run(t, options, "check-readiness", nil)
	if result["network_checked"] != false {
		t.Fatal("network check implied")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "private-key") || strings.Contains(string(raw), "example.test") {
		t.Fatal("credentials or endpoint leaked")
	}
	after, _ := os.ReadFile(options.Config)
	if string(before) != string(after) {
		t.Fatal("readiness changed config")
	}
	if result["config_revision"] == "" {
		t.Fatal("missing revision")
	}
}

func TestReadinessReportsMissingCredentialsAndAuthorization(t *testing.T) {
	t.Setenv("MVP_TEST_BRAVE", "")
	data := map[string]any{"models": []any{map[string]any{"name": "first", "model": "fixture", "provider": "openai", "api_key": "${MVP_TEST_BRAVE:-not-configured}"}}, "tools": []any{map[string]any{"name": "web_search", "api_key": "${MVP_TEST_BRAVE:-}"}}}
	states := map[string]string{}
	for _, item := range capabilityReadiness(data, t.TempDir()) {
		states[item["id"]] = item["state"]
	}
	if states["model"] != "missing_credential" || states["web_search"] != "missing_credential" || states["commands"] != "not_authorized" || states["host_opencli"] != "not_authorized" {
		t.Fatal(states)
	}
	t.Setenv("MVP_TEST_BRAVE", "fixture-secret")
	for _, item := range capabilityReadiness(data, t.TempDir()) {
		if (item["id"] == "model" || item["id"] == "web_search") && item["state"] != "unverified" {
			t.Fatal("local config cannot prove connectivity", item)
		}
	}
}

func TestReadinessDoesNotInitializeDataAndFollowsModelSelection(t *testing.T) {
	options := setup(t)
	if _, err := Run(options, "check-readiness", nil); err == nil {
		t.Fatal("missing saved config accepted")
	}
	if _, err := os.Stat(options.UserData); !os.IsNotExist(err) {
		t.Fatal("readiness initialized user data", err)
	}
	data := map[string]any{"default_model": "second", "local_acp": map[string]any{"model_name": ""}, "models": []any{
		map[string]any{"name": "first", "provider": "openai", "model": "fixture", "api_key": ""},
		map[string]any{"name": "second", "provider": "openai", "model": "fixture", "api_key": "test-only"},
	}, "tools": []any{map[string]any{"name": "web_search", "api_key": "test-only"}}}
	if capabilityReadiness(data, t.TempDir())[0]["state"] != "unverified" {
		t.Fatal("empty local model failed to fall back")
	}
	data["api"] = map[string]any{"model_name": "first"}
	if capabilityReadiness(data, t.TempDir())[0]["state"] != "missing_credential" {
		t.Fatal("legacy selected model ignored")
	}
	readObject(data, "local_acp")["tool_allowlist"] = []any{}
	for _, item := range capabilityReadiness(data, t.TempDir()) {
		if item["id"] != "model" && item["state"] != "not_authorized" {
			t.Fatal("empty allowlist widened", item)
		}
	}
}

func TestReadinessResolvesOpenCLIRelativeToConfigWithoutExecuting(t *testing.T) {
	t.Setenv("OPENCLI_BIN", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture-cli.exe"), []byte("not an executable"), 0600); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"sandbox": map[string]any{"allow_host_tools": true}, "tools": []any{map[string]any{"name": "host_opencli", "executable": "./fixture-cli.exe"}}}
	for _, item := range capabilityReadiness(data, dir) {
		if item["id"] == "host_opencli" && item["state"] != "unverified" {
			t.Fatal("relative configured launcher not resolved", item)
		}
	}
}
