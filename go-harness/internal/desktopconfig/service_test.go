package desktopconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = `api:
  deerflow_home: ../data/deerflow
  extensions_config_path: ./extensions_config.json
local_acp:
  permission_mode: dangerous
  memory_scope: workspace
  max_active_connections: 16
  max_active_runs: 2
  queue_timeout_seconds: 600
  run_timeout_seconds: 600
  session_cleanup_enabled: false
  inactive_session_retention_days: 30
  closed_session_retention_days: 30
  session_cleanup_interval_seconds: 3600
models:
  - name: first
    use: langchain_openai:ChatOpenAI
    model: gpt-a
    api_key: first-private-key
    base_url: https://example.test/v1
  - name: second
    use: langchain_openai:ChatOpenAI
    model: gpt-b
    api_key: second-private-key
    base_url: https://example.test/v1
default_model: first
sandbox:
  use: deerflow.sandbox.local:LocalSandboxProvider
  allow_host_bash: false
  allow_host_tools: false
skills:
  enabled: true
  path: ../skills
  extensions_file: ./extensions_config.json
subagents:
  enabled: true
  agents: {}
  custom_agents: {}
skill_evolution:
  enabled: false
  mode: review
memory:
  enabled: true
  manager_class: deermem
  mode: middleware
  injection_enabled: true
  shutdown_flush_timeout_seconds: 30
  backend_config:
    storage_path: memory.json
    storage_class: deerflow.agents.memory.storage.FileMemoryStorage
    debounce_seconds: 30
    max_facts: 100
    fact_confidence_threshold: 0.7
    fact_dedup_enabled: false
    fact_dedup_similarity_threshold: 0.7
    max_injection_tokens: 2000
    retrieval_enabled: true
    retrieval_top_k: 12
    retrieval_index_path: memory-fts5.sqlite3
tool_groups:
  - name: file:read
tools:
  - name: read_file
    group: file:read
    use: deerflow.sandbox.tools:read_file_tool
`

func setup(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	resources := filepath.Join(root, "resources")
	if err := os.MkdirAll(resources, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "default-config.yaml"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	return Options{Config: filepath.Join(root, "user-data", "config", "config.yaml"), UserData: filepath.Join(root, "user-data"), Resources: resources}
}

func run(t *testing.T, options Options, operation string, input any) map[string]any {
	t.Helper()
	var raw []byte
	if input != nil {
		var err error
		raw, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
	}
	value, err := Run(options, operation, raw)
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s result type %T", operation, value)
	}
	return result
}

func modelByName(t *testing.T, data map[string]any, name string) map[string]any {
	t.Helper()
	for _, item := range list(data, "models") {
		model := item.(map[string]any)
		if str(model, "name", "") == name {
			return model
		}
	}
	t.Fatalf("model %s missing", name)
	return nil
}

func TestInitSnapshotSaveAndRepeatOnSamePaths(t *testing.T) {
	options := setup(t)
	init := run(t, options, "init", nil)
	if init["initialized"] != true {
		t.Fatal(init)
	}
	doc := run(t, options, "snapshot", nil)
	if doc["config_revision"] == "" || doc["extensions_revision"] == "" {
		t.Fatal("missing revisions")
	}
	visible, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(visible), "first-private-key") || strings.Contains(string(visible), "second-private-key") {
		t.Fatal("snapshot exposed model credentials")
	}
	if modelByName(t, doc, "first")["api_key"] != "" {
		t.Fatal("literal key must be hidden")
	}
	if run(t, options, "validate", doc)["valid"] != true {
		t.Fatal("validation failed")
	}
	first := run(t, options, "save", doc)
	second := run(t, options, "save", first)
	if second["backup"] == "" {
		t.Fatal("backup missing")
	}
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if str(raw["models"].([]any)[0].(map[string]any), "api_key", "") != "first-private-key" {
		t.Fatal("first credential changed")
	}
	if str(raw["models"].([]any)[1].(map[string]any), "api_key", "") != "second-private-key" {
		t.Fatal("second credential changed")
	}
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("stale revision accepted")
	}
}

func TestModelReorderAndBackendChangeDoNotTransferSecrets(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	models := list(doc, "models")
	doc["models"] = []any{models[1], models[0]}
	result := run(t, options, "save", doc)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	saved := list(raw, "models")
	if str(saved[0].(map[string]any), "api_key", "") != "second-private-key" || str(saved[1].(map[string]any), "api_key", "") != "first-private-key" {
		t.Fatal("reorder transferred a secret")
	}
	first := modelByName(t, result, "first")
	first["base_url"] = "https://other.test/v1"
	changed := run(t, options, "save", result)
	if modelByName(t, changed, "first")["api_key_configured"] != false {
		t.Fatal("endpoint change retained old credential")
	}
	raw, err = readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list(raw, "models") {
		model := item.(map[string]any)
		if str(model, "name", "") == "first" && str(model, "api_key", "") != "" {
			t.Fatal("endpoint change persisted old credential")
		}
	}
}

func TestExistingPatchedOpenAIModelCanBeSaved(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	first := list(raw, "models")[0].(map[string]any)
	first["use"] = "deerflow.models.patched_openai:PatchedChatOpenAI"
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	if run(t, options, "validate", doc)["valid"] != true {
		t.Fatal("existing patched OpenAI model could not be validated")
	}
	run(t, options, "save", doc)
	saved, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if str(list(saved, "models")[0].(map[string]any), "provider", "") != "openai" || str(list(saved, "models")[0].(map[string]any), "use", "") != "" {
		t.Fatal("legacy provider was not normalized to a native Go provider")
	}
}

func TestChangedOriginalNameRequiresCredential(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	first := modelByName(t, doc, "first")
	first["original_name"] = "missing"
	result := run(t, options, "save", doc)
	if modelByName(t, result, "first")["api_key_configured"] != false {
		t.Fatal("unknown original retained credential")
	}
}

func TestNestedSecretsAreRedactedAndRestored(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	object(readObject(raw, "memory"), "backend_config")["access_token"] = "memory-private-token"
	object(raw, "subagents")["custom_agents"] = map[string]any{"writer": map[string]any{"model": "inherit", "api_key": "subagent-private-key"}}
	object(raw, "skill_evolution")["access_token"] = "evolution-private-token"
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(options.UserData, "data", "deerflow", "agents", "writer")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte("name: writer\nmodel: first\nsecret_key: agent-private-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	visible, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"memory-private-token", "subagent-private-key", "evolution-private-token", "agent-private-key"} {
		if strings.Contains(string(visible), secret) {
			t.Fatal("snapshot exposed a nested credential")
		}
	}
	if run(t, options, "validate", doc)["valid"] != true {
		t.Fatal("nested secret validation failed")
	}
	run(t, options, "save", doc)
	saved, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if str(readObject(readObject(saved, "memory"), "backend_config"), "access_token", "") != "memory-private-token" {
		t.Fatal("memory secret changed")
	}
	if str(readObject(readObject(readObject(saved, "subagents"), "custom_agents"), "writer"), "api_key", "") != "subagent-private-key" {
		t.Fatal("subagent secret changed")
	}
	if str(readObject(saved, "skill_evolution"), "access_token", "") != "evolution-private-token" {
		t.Fatal("evolution secret changed")
	}
	agent, err := readYAML(filepath.Join(agentDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if str(agent, "secret_key", "") != "agent-private-key" {
		t.Fatal("agent secret changed")
	}
}

func TestBridgePolicyRequiresExistingAbsoluteCommand(t *testing.T) {
	options := setup(t)
	policy := run(t, options, "bridge-policy", nil)
	if policy["enabled"] != false {
		t.Fatal(policy)
	}
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	local := object(raw, "local_acp")
	local["accept_client_mcp_servers"] = true
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	policy = run(t, options, "bridge-policy", nil)
	if policy["enabled"] != false || len(policy["allowed_commands"].([]string)) != 0 {
		t.Fatalf("missing allowlist should disable optional MCP: %v", policy)
	}
	local["client_mcp_allowed_commands"] = []any{}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	policy = run(t, options, "bridge-policy", nil)
	if policy["enabled"] != false || len(policy["allowed_commands"].([]string)) != 0 {
		t.Fatalf("empty allowlist should disable optional MCP: %v", policy)
	}
	local["client_mcp_allowed_commands"] = []any{command}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	policy = run(t, options, "bridge-policy", nil)
	if policy["enabled"] != true || len(policy["allowed_commands"].([]string)) != 1 {
		t.Fatal(policy)
	}
	local["client_mcp_allowed_commands"] = []any{"relative-command"}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(options, "bridge-policy", nil); err == nil {
		t.Fatal("relative MCP command accepted")
	}
	local["client_mcp_allowed_commands"] = "not-an-array"
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(options, "bridge-policy", nil); err == nil {
		t.Fatal("malformed MCP allowlist accepted")
	}
}

func TestSaveWithUnconfiguredClientMCPStillApplies(t *testing.T) {
	for _, allowlist := range []struct {
		name  string
		value any
	}{
		{name: "missing"},
		{name: "empty", value: []any{}},
	} {
		t.Run(allowlist.name, func(t *testing.T) {
			options := setup(t)
			run(t, options, "init", nil)
			raw, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			local := object(raw, "local_acp")
			local["accept_client_mcp_servers"] = true
			if allowlist.value != nil {
				local["client_mcp_allowed_commands"] = allowlist.value
			}
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			doc := run(t, options, "snapshot", nil)
			if run(t, options, "validate", doc)["valid"] != true {
				t.Fatal("validation failed")
			}
			saved := run(t, options, "save", doc)
			if saved["backup"] == "" {
				t.Fatal("save omitted backup")
			}
			policy := run(t, options, "bridge-policy", nil)
			if policy["enabled"] != false || len(policy["allowed_commands"].([]string)) != 0 {
				t.Fatalf("unconfigured client MCP gained authority: %v", policy)
			}
			if run(t, options, "validate", saved)["valid"] != true {
				t.Fatal("saved document did not round-trip")
			}
		})
	}
}

func TestArraySecretRestoreUsesStableIdentity(t *testing.T) {
	old := []any{
		map[string]any{"name": "first", "command": "one", "api_key": "first-private"},
		map[string]any{"name": "second", "command": "two", "api_key": "second-private"},
	}
	incoming := []any{
		map[string]any{"name": "second", "command": "two", "api_key": redacted},
		map[string]any{"name": "first", "command": "one", "api_key": redacted},
	}
	restored := restore(incoming, old).([]any)
	if str(restored[0].(map[string]any), "api_key", "") != "second-private" || str(restored[1].(map[string]any), "api_key", "") != "first-private" {
		t.Fatal("array reorder transferred a secret")
	}
	incoming[0].(map[string]any)["command"] = "changed"
	if !hasRedacted(restore(incoming, old)) {
		t.Fatal("command change inherited old secret")
	}
	if !hasRedacted(restore([]any{redacted}, []any{"private"})) {
		t.Fatal("anonymous array inherited a secret by index")
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
