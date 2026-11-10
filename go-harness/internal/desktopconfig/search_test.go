package desktopconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
)

func searchOperation(t *testing.T, options Options, operation string, request any) websearch.Status {
	t.Helper()
	body, _ := json.Marshal(request)
	value, err := Run(options, operation, body)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := value.(websearch.Status)
	if !ok {
		t.Fatalf("unexpected search status %T", value)
	}
	return state
}

func TestFirstInstallSearchPluginsInitializeSwitchAndPreserveConfig(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "resources/default-config.yaml", "resources/desktop-default-config.yaml", "desktop-app/resources/deerflow/default-config.yaml"} {
		t.Run(name, func(t *testing.T) {
			for _, env := range []string{"BRAVE_API_KEY", "DEEPSEEK_API_KEY", "SEARXNG_BASE_URL", "SEARXNG_API_KEY", "SEARXNG_TOKEN"} {
				t.Setenv(env, "")
			}
			t.Setenv("USERPROFILE", t.TempDir())
			template, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			options := setup(t)
			if err := os.WriteFile(filepath.Join(options.Resources, "default-config.yaml"), template, 0600); err != nil {
				t.Fatal(err)
			}
			run(t, options, "init", nil)
			before, err := os.ReadFile(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(options.Config), "search-providers.json")); err != nil {
				t.Fatal("initialization did not create the search store", err)
			}
			initial, _ := readYAML(options.Config)
			for _, key := range []string{"api_key", "https_proxy", "timeout"} {
				if _, exists := toolByName(t, initial, "web_search")[key]; exists {
					t.Fatal("first install contains obsolete search field", key)
				}
			}
			state := searchOperation(t, options, "search-status", nil)
			if len(state.Providers) != 3 || state.FetchProvider != "local" {
				t.Fatal("first install omitted search plugins")
			}
			for _, provider := range state.Providers {
				if provider.Configured {
					t.Fatal("fresh profile inherited personal credentials")
				}
			}
			var cfg deerflow.Config
			explicit := map[string]bool{}
			if runtime.GOOS != "windows" {
				// Desktop templates enable Windows shell mapping. Exercise their
				// search initialization on Linux through the explicit provider path;
				// this test never executes a shell.
				cfg.Sandbox = harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true}
				explicit["sandbox-provider"] = true
			}
			if _, err := launch.ApplyConfig(options.Config, &cfg, new(int), explicit); err != nil {
				t.Fatal(err)
			}
			if cfg.SearchSettingsPath != filepath.Join(filepath.Dir(options.Config), "search-providers.json") {
				t.Fatal("daemon and config CLI use different search stores")
			}
			// Keep one manager alive while the CLI changes the profile, proving the
			// same initialized configuration is observed without daemon restart.
			daemon, err := websearch.New(cfg.SearchSettingsPath, cfg.Search)
			if err != nil {
				t.Fatal(err)
			}
			for _, provider := range []string{"deepseek", "searxng", "brave"} {
				request := map[string]any{"provider": provider, "expected_revision": state.Revision, "make_default": true}
				if provider == "searxng" {
					request["base_url"] = "https://search.example.test"
				} else {
					request["api_key"] = "fixture-" + provider + "-key"
				}
				state = searchOperation(t, options, "search-configure", request)
				actual, err := daemon.Status()
				if err != nil || actual.DefaultProvider != provider {
					t.Fatal("live provider update was not observed", provider, err)
				}
			}
			state = searchOperation(t, options, "search-select", map[string]any{"provider": "searxng"})
			for _, p := range state.Providers {
				if !p.Configured {
					t.Fatal("switch lost a configuration")
				}
				if p.Name == "searxng" && p.APIKeyConfigured {
					t.Fatal("SearXNG required a Key")
				}
			}
			run(t, options, "init", nil)
			state = searchOperation(t, options, "search-status", nil)
			if state.DefaultProvider != "searxng" {
				t.Fatal("initialization overwrote the selected plugin")
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("switch or initialization rewrote existing YAML")
			}
		})
	}
}

func storedSearchProvider(t *testing.T, options Options, name string) map[string]any {
	t.Helper()
	data, err := readJSON(filepath.Join(filepath.Dir(options.Config), "search-providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	return readObject(readObject(readObject(data, "config"), "providers"), name)
}

func TestLegacySearchMigrationPersistsBeforeRemovingFieldsAndIsIdempotent(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")
	options := setup(t)
	run(t, options, "init", nil)
	raw, _ := readYAML(options.Config)
	raw["tools"] = append(list(raw, "tools"),
		map[string]any{"name": "web_search", "api_key": "legacy-fixture-key", "https_proxy": "http://127.0.0.1:11808", "timeout": 17, "max_results": 7},
		map[string]any{"name": "web_fetch", "timeout": 11})
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(options.Config)
	original = append([]byte("# preserve this user comment\n"), original...)
	if err := os.WriteFile(options.Config, original, 0600); err != nil {
		t.Fatal(err)
	}
	run(t, options, "init", nil)
	provider := storedSearchProvider(t, options, "brave")
	if provider["api_key"] != "legacy-fixture-key" || provider["https_proxy"] != "http://127.0.0.1:11808" || number(provider, "timeout", 0) != 17 {
		t.Fatal("legacy search settings were not durably migrated")
	}
	after, _ := readYAML(options.Config)
	for _, key := range []string{"api_key", "https_proxy", "timeout"} {
		if _, exists := toolByName(t, after, "web_search")[key]; exists {
			t.Fatal("obsolete field survived migration", key)
		}
	}
	if toolByName(t, after, "web_search")["max_results"] != 7 || toolByName(t, after, "web_fetch")["timeout"] != 11 || !reflect.DeepEqual(after["models"], raw["models"]) {
		t.Fatal("migration changed unrelated configuration")
	}
	backup, err := os.ReadFile(options.Config + ".search-migration.bak")
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatal("migration did not preserve the exact original", err)
	}
	cleaned, _ := os.ReadFile(options.Config)
	if !bytes.Contains(cleaned, []byte("# preserve this user comment")) {
		t.Fatal("migration dropped YAML comments")
	}
	storePath := filepath.Join(filepath.Dir(options.Config), "search-providers.json")
	store, _ := os.ReadFile(storePath)
	run(t, options, "init", nil)
	again, _ := os.ReadFile(options.Config)
	storeAgain, _ := os.ReadFile(storePath)
	if !bytes.Equal(cleaned, again) || !bytes.Equal(store, storeAgain) {
		t.Fatal("repeated initialization rewrote migrated data")
	}
}

func TestSearchMigrationKeepsExistingProviderAndExplicitCredentials(t *testing.T) {
	options := setup(t)
	state := searchOperation(t, options, "search-status", nil)
	state = searchOperation(t, options, "search-configure", map[string]any{"provider": "brave", "expected_revision": state.Revision, "api_key": "new-fixture-key"})
	searchOperation(t, options, "search-configure", map[string]any{"provider": "searxng", "expected_revision": state.Revision, "base_url": "https://search.example.test", "make_default": true})
	raw, _ := readYAML(options.Config)
	raw["tools"] = append(list(raw, "tools"), map[string]any{"name": "web_search", "api_key": "obsolete-fixture-key"})
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	state = searchOperation(t, options, "search-status", nil)
	if state.DefaultProvider != "searxng" || storedSearchProvider(t, options, "brave")["api_key"] != "new-fixture-key" {
		t.Fatal("migration replaced an existing search preference or explicit key")
	}
}

func TestSearchMigrationFailureKeepsOriginalFields(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, _ := readYAML(options.Config)
	raw["tools"] = append(list(raw, "tools"), map[string]any{"name": "web_search", "api_key": "legacy-fixture-key"})
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(options.Config)
	if err := os.WriteFile(filepath.Join(filepath.Dir(options.Config), "search-providers.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(options, "init", nil); err == nil {
		t.Fatal("migration ignored a failed search-store write")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(original, after) {
		t.Fatal("failed migration removed the original credentials")
	}
}

func TestSearchConfigurationDoesNotInvalidateOrOverwriteSettingsDraft(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	state := searchOperation(t, options, "search-status", nil)
	searchOperation(t, options, "search-configure", map[string]any{"provider": "searxng", "expected_revision": state.Revision, "base_url": "https://search.example.test", "make_default": true})
	// Saving an unrelated, previously opened settings draft must retain the
	// separately saved selection and credentials without requiring a rebase.
	run(t, options, "save", doc)
	state = searchOperation(t, options, "search-status", nil)
	if state.DefaultProvider != "searxng" {
		t.Fatal("settings save reverted independent selection")
	}
}
