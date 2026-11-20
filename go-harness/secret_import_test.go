package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
)

type importFixture struct {
	vault          *secrets.Vault
	manage         *secretTool
	config, search string
	values         map[string]string
}

func newImportFixture(t *testing.T) importFixture {
	t.Helper()
	ctx := context.Background()
	for _, name := range []string{"BRAVE_API_KEY", "DEEPSEEK_API_KEY", "SEARXNG_BASE_URL", "SEARXNG_API_KEY", "SEARXNG_TOKEN"} {
		t.Setenv(name, "")
	}
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := testutil.CanonicalTempDir(t)
	f := importFixture{config: filepath.Join(dir, "config.yaml"), search: filepath.Join(dir, "search-providers.json"), values: map[string]string{}}
	for _, name := range []string{"model", "tool", "proxy", "feishu", "deepseek", "other"} {
		f.values[name] = randomTestSecret(t, name+"-")
	}
	raw := strings.Join([]string{
		"# DeerFlow host configuration",
		"default_model: deepseek",
		"models:",
		"  - name: deepseek",
		"    use: langchain_openai:ChatOpenAI",
		"    model: deepseek-chat",
		"    api_key: " + f.values["model"] + "   # primary",
		"    base_url: https://models.example/v1",
		"  - name: backup",
		"    use: langchain_openai:ChatOpenAI",
		"    model: backup-model",
		`    api_key: "` + f.values["model"] + `"`,
		"    base_url: https://backup.example/v1",
		"tools:",
		"  - name: web_search",
		"    api_key: " + f.values["tool"],
		"network:",
		"  proxy:",
		"    enabled: true",
		"    url: http://user:" + f.values["proxy"] + "@proxy.example:3128",
		"channels:",
		"  feishu:",
		"    app_secret: " + f.values["feishu"],
		"",
	}, "\r\n")
	if err := os.WriteFile(f.config, []byte(raw), 0o640); err != nil {
		t.Fatal(err)
	}
	manager, err := websearch.New(f.search, harness.SearchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status()
	if err != nil {
		t.Fatal(err)
	}
	tool, deepseek := f.values["tool"], f.values["deepseek"]
	if status, err = manager.Update(ctx, websearch.Update{Provider: "brave", ExpectedRevision: status.Revision, APIKey: &tool}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Update(ctx, websearch.Update{Provider: "deepseek", ExpectedRevision: status.Revision, APIKey: &deepseek}); err != nil {
		t.Fatal(err)
	}
	// A run pinned these plaintext versions before the import.
	if _, err := manager.Snapshot(nil); err != nil {
		t.Fatal(err)
	}
	vault, _, _ := secretToolFixture(t, harness.Session{ID: "s1", CWD: dir, Mode: "default"})
	items, _, err := secretTools(vault, harness.RunRequest{Session: harness.Session{ID: "s1", CWD: dir, Mode: "default"}, RunID: "run"}, secretSources{config: f.config, search: manager})
	if err != nil {
		t.Fatal(err)
	}
	f.vault, f.manage = vault, items[1].(*secretTool)
	// Names the import must not take over: another value, and a deleted
	// secret. A deleted secret is never reused, even with the same value.
	for name, value := range map[string]string{"WEB_SEARCH_API_KEY": f.values["other"], "NETWORK_PROXY_URL": f.values["other"], "EXISTING_DEEPSEEK": f.values["deepseek"], "OLD_MODEL_KEY": f.values["model"]} {
		if _, err := vault.Set(ctx, secrets.SetRequest{Name: name, Kind: secrets.KindString, Value: []byte(value), Actor: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"NETWORK_PROXY_URL", "OLD_MODEL_KEY"} {
		if _, err := vault.Delete(ctx, name, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f importFixture) call(t *testing.T, args map[string]any) (map[string]any, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	out, err := f.manage.InvokableRun(context.Background(), string(b))
	for name, value := range f.values {
		if strings.Contains(out, value) || err != nil && strings.Contains(err.Error(), value) {
			t.Fatalf("%v exposed the %s value", args, name)
		}
	}
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded, nil
}

func importedSettings(t *testing.T, out map[string]any) map[string]string {
	t.Helper()
	got := map[string]string{}
	items, _ := out["imported"].([]any)
	for _, item := range items {
		entry := item.(map[string]any)
		got[entry["field"].(string)] = entry["secret"].(string) + " " + entry["action"].(string)
	}
	return got
}

func TestSecretImportConfigMovesPlaintextCredentials(t *testing.T) {
	ctx := context.Background()
	f := newImportFixture(t)
	before, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	beforeList, _ := f.vault.List(ctx, true)

	out, err := f.call(t, map[string]any{"action": "import_config", "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"models[deepseek].api_key":                         "MODEL_DEEPSEEK_API_KEY create",
		"models[backup].api_key":                           "MODEL_DEEPSEEK_API_KEY reuse",
		"tools[web_search].api_key":                        "WEB_SEARCH_API_KEY_2 create",
		"network.proxy.url":                                "NETWORK_PROXY_URL_2 create",
		"search-providers.json:providers.brave.api_key":    "WEB_SEARCH_API_KEY_2 reuse",
		"search-providers.json:providers.deepseek.api_key": "EXISTING_DEEPSEEK reuse",
	}
	if got := importedSettings(t, out); out["status"] != "dry_run" || len(got) != len(want) {
		t.Fatalf("dry run = %v", out)
	} else {
		for field, action := range want {
			if got[field] != action {
				t.Errorf("dry run %s = %q; want %q", field, got[field], action)
			}
		}
	}
	unchanged, _ := json.Marshal(out["unchanged"])
	if !strings.Contains(string(unchanged), "channels.feishu.app_secret") || !strings.Contains(string(unchanged), "search-providers.json.versions") {
		t.Fatalf("unchanged = %s", unchanged)
	}
	after, _ := os.ReadFile(f.config)
	afterList, _ := f.vault.List(ctx, true)
	if string(after) != string(before) || len(afterList) != len(beforeList) {
		t.Fatal("a dry run changed config.yaml or the vault")
	}

	for _, args := range []map[string]any{
		{"action": "import_config", "fields": []string{"channels.feishu.app_secret"}},
		{"action": "import_config", "fields": []string{}},
		{"action": "import_config", "name": "X"},
		{"action": "set", "name": "X", "value": "some-value-123", "dry_run": true},
		{"action": "delete", "name": "X", "fields": []string{"network.proxy.url"}},
		// name is optional only for import_config.
		{"action": "set", "value": "some-value-123"},
		{"action": "delete"},
		{"action": "restore"},
		{"action": "reveal"},
	} {
		if _, err := f.call(t, args); err == nil || !secretNotExecuted(err) {
			t.Errorf("%v = %v; want not executed", args, err)
		}
	}

	// Importing one setting leaves the others as they are.
	out, err = f.call(t, map[string]any{"action": "import_config", "fields": []string{"network.proxy.url"}})
	if err != nil || out["status"] != "imported" || importedSettings(t, out)["network.proxy.url"] != "NETWORK_PROXY_URL_2 created" || len(importedSettings(t, out)) != 1 {
		t.Fatalf("partial import = %v, %v", out, err)
	}
	out, err = f.call(t, map[string]any{"action": "import_config"})
	if err != nil || out["status"] != "imported" {
		t.Fatalf("import = %v, %v", out, err)
	}
	for field, action := range want {
		if field == "network.proxy.url" {
			continue
		}
		action = strings.Replace(strings.Replace(action, " create", " created", 1), " reuse", " reused", 1)
		if got := importedSettings(t, out)[field]; got != action {
			t.Errorf("import %s = %q; want %q", field, got, action)
		}
	}
	if resolveSecret(t, f.vault, "MODEL_DEEPSEEK_API_KEY") != f.values["model"] || resolveSecret(t, f.vault, "WEB_SEARCH_API_KEY_2") != f.values["tool"] || resolveSecret(t, f.vault, "WEB_SEARCH_API_KEY") != f.values["other"] || !strings.Contains(resolveSecret(t, f.vault, "NETWORK_PROXY_URL_2"), f.values["proxy"]) {
		t.Fatal("imported values are not in the vault")
	}
	meta, err := f.vault.Get(ctx, "MODEL_DEEPSEEK_API_KEY", false)
	if err != nil || meta.Description != "Imported from config.yaml models[deepseek].api_key" {
		t.Fatalf("description = %q, %v", meta.Description, err)
	}

	rewritten, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	for _, fragment := range []string{"# DeerFlow host configuration\r\n", "api_key: ${secret:MODEL_DEEPSEEK_API_KEY}   # primary\r\n", `api_key: "${secret:MODEL_DEEPSEEK_API_KEY}"` + "\r\n", "api_key: ${secret:WEB_SEARCH_API_KEY_2}\r\n", "url: ${secret:NETWORK_PROXY_URL_2}\r\n", "app_secret: " + f.values["feishu"] + "\r\n"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("config.yaml lacks %q", fragment)
		}
	}
	for _, name := range []string{"model", "tool", "proxy"} {
		if strings.Contains(text, f.values[name]) {
			t.Errorf("config.yaml still holds the %s value", name)
		}
	}
	if info, err := os.Stat(f.config); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("config.yaml mode = %v, %v", info.Mode(), err)
	}
	settings, err := os.ReadFile(f.search)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(settings), f.values["tool"]) || strings.Contains(string(settings), f.values["deepseek"]) || !strings.Contains(string(settings), "${secret:WEB_SEARCH_API_KEY_2}") || !strings.Contains(string(settings), "${secret:EXISTING_DEEPSEEK}") {
		t.Fatalf("search settings = %s", settings)
	}
	if left, err := configschema.FindPlaintextCredentials(rewritten); err != nil || len(left) != 1 || left[0].Field != "channels.feishu.app_secret" {
		t.Fatalf("plaintext left = %+v, %v", left, err)
	}
	if out, err := f.call(t, map[string]any{"action": "import_config"}); err != nil || out["status"] != "nothing_to_import" {
		t.Fatalf("second import = %v, %v", out, err)
	}

	// Changing or deleting a secret the settings refer to says so.
	out, err = f.call(t, map[string]any{"action": "set", "name": "WEB_SEARCH_API_KEY_2", "value": randomTestSecret(t, "rotated-")})
	if note, _ := out["config_note"].(string); err != nil || !strings.Contains(note, "tools[web_search].api_key") || !strings.Contains(note, "search-providers.json:providers.brave.api_key") {
		t.Fatalf("set of a referenced secret = %v, %v", out, err)
	}
	out, err = f.call(t, map[string]any{"action": "delete", "name": "MODEL_DEEPSEEK_API_KEY"})
	if warning, _ := out["warning"].(string); err != nil || !strings.Contains(warning, "models[deepseek].api_key, models[backup].api_key") {
		t.Fatalf("delete of a referenced secret = %v, %v", out, err)
	}
	if out, err := f.call(t, map[string]any{"action": "delete", "name": "EXISTING_DEEPSEEK"}); err != nil || !strings.Contains(out["warning"].(string), "providers.deepseek.api_key") {
		t.Fatalf("delete of a search secret = %v, %v", out, err)
	}
}

func TestSecretImportConfigWaitsForOtherConfigWriters(t *testing.T) {
	f := newImportFixture(t)
	before, _ := os.ReadFile(f.config)
	lock := flock.New(filepath.Join(filepath.Dir(f.config), ".config-tool.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	// A caller that stops waiting, or a writer that keeps the lock too long,
	// leaves everything as it was.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.manage.InvokableRun(ctx, `{"action":"import_config"}`); err == nil || !secretNotExecuted(err) {
		t.Fatalf("import cancelled while another writer holds the lock = %v", err)
	}
	defer func(wait time.Duration) { configLockWait = wait }(configLockWait)
	configLockWait = 200 * time.Millisecond
	if _, err := f.manage.InvokableRun(context.Background(), `{"action":"import_config"}`); err == nil || !secretNotExecuted(err) || !strings.Contains(err.Error(), "retry shortly") {
		t.Fatalf("import while another writer keeps the lock = %v", err)
	}
	configLockWait = 10 * time.Second
	// Otherwise the import waits for the other writer, then runs.
	done := make(chan error, 1)
	go func() {
		_, err := f.manage.InvokableRun(context.Background(), `{"action":"import_config","fields":["network.proxy.url"]}`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("import ran while another writer held the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if after, _ := os.ReadFile(f.config); string(after) != string(before) {
		t.Fatal("config.yaml changed while another writer held the lock")
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("import after the other writer finished = %v", err)
	}
	if after, _ := os.ReadFile(f.config); !strings.Contains(string(after), "url: ${secret:NETWORK_PROXY_URL_2}") {
		t.Fatal("the import did not run after the other writer finished")
	}

	// A file that changed after it was read is never replaced.
	path := f.config
	changed := append([]byte("# edited\n"), before...)
	if err := os.WriteFile(path, changed, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := replaceConfigFile(path, before, []byte("replaced: true\n"), 0o640); err == nil || !strings.Contains(err.Error(), "changed while the import ran") {
		t.Fatalf("replace after a change = %v", err)
	}
	if current, _ := os.ReadFile(path); string(current) != string(changed) {
		t.Fatal("a changed config.yaml was replaced")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".deerflow-config-") {
			t.Fatalf("temporary file %s was left behind", entry.Name())
		}
	}

	// Without a config.yaml there is nothing to import.
	none := *f.manage
	none.sources = secretSources{}
	if _, err := none.InvokableRun(context.Background(), `{"action":"import_config","dry_run":true}`); err == nil || !secretNotExecuted(err) || !strings.Contains(err.Error(), "without a config.yaml") {
		t.Fatalf("import without config = %v", err)
	}

	// Sessions that may only read secrets cannot import.
	for _, session := range []harness.Session{{ID: "s1", Mode: "plan"}, {ID: "s1", Mode: "default", ApprovalMode: harness.ApprovalReadOnly}, {ID: "s1", Mode: "default", ApprovalMode: harness.ApprovalRejectAlways}} {
		limited := *f.manage
		limited.run.Session = session
		if _, err := limited.InvokableRun(context.Background(), `{"action":"import_config","dry_run":true}`); !errors.Is(err, harness.ErrPermissionDenied) || !secretNotExecuted(err) {
			t.Fatalf("import in a %s/%s session = %v", session.Mode, session.ApprovalMode, err)
		}
	}
}

func TestSecretImportConfigReportsStoredSecretsWhenTheRewriteFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test user cannot write")
	}
	f := newImportFixture(t)
	field := "models[deepseek].api_key"
	if _, err := f.call(t, map[string]any{"action": "import_config", "dry_run": true, "fields": []string{field}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f.config)
	dir := filepath.Dir(f.config)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := f.call(t, map[string]any{"action": "import_config", "fields": []string{field}})
	if err == nil || secretNotExecuted(err) || !strings.Contains(err.Error(), "the secrets are stored, but config.yaml was not changed") {
		t.Fatalf("import into a read-only directory = %v", err)
	}
	if after, _ := os.ReadFile(f.config); string(after) != string(before) {
		t.Fatal("config.yaml changed")
	}
	if resolveSecret(t, f.vault, "MODEL_DEEPSEEK_API_KEY") != f.values["model"] {
		t.Fatal("the secret was not stored")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := f.call(t, map[string]any{"action": "import_config", "fields": []string{field}})
	if err != nil || importedSettings(t, out)[field] != "MODEL_DEEPSEEK_API_KEY reused" {
		t.Fatalf("retry = %v, %v", out, err)
	}
}

// Legacy tools[web_search].https_proxy feeds search.providers.brave.https_proxy,
// which websearch.Validate forbids as a secret reference. import_config must
// leave it untouched so startup and desktopconfig continue to succeed.
func TestImportConfigSkipsLegacyWebSearchHTTPSProxy(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	proxyVal := "http://user:" + randomTestSecret(t, "wsp-") + "@proxy.example:8080"
	raw := "default_model: m\nmodels:\n  - name: m\n    provider: openai\n    model: m\n    api_key: " + randomTestSecret(t, "mk-") + "\ntools:\n  - name: web_search\n    https_proxy: " + proxyVal + "\n"
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	vault, _, _ := secretToolFixture(t, harness.Session{ID: "s1", CWD: dir, Mode: "default"})
	items, _, err := secretTools(vault, harness.RunRequest{Session: harness.Session{ID: "s1", CWD: dir, Mode: "default"}, RunID: "run"}, secretSources{config: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	outRaw, err := items[1].(*secretTool).InvokableRun(context.Background(), `{"action":"import_config"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outRaw, "WEB_SEARCH_HTTPS_PROXY") {
		t.Fatalf("legacy web_search https_proxy should not be imported: %s", outRaw)
	}
	rewritten, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := websearch.SeedYAML(rewritten)
	if err != nil {
		t.Fatalf("SeedYAML after import failed: %v", err)
	}
	if err := websearch.Validate(seed); err != nil {
		t.Fatalf("websearch.Validate after import failed: %v", err)
	}
}
