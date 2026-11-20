package desktopconfig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/runtimepaths"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

// seedProfileSecrets stores values in the vault of the portable profile's
// runtime database and binds it, as the first runtime start does.
func seedProfileSecrets(t *testing.T, options Options, values map[string]string) {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(options.UserData, "config", "go-harness-state")
	store, err := sqlite.Open(runtimepaths.Database(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := secrets.Open(ctx, store.DB(), runtimepaths.SecretVault(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	for name, value := range values {
		if _, err := vault.Set(ctx, secrets.SetRequest{Name: name, Kind: secrets.KindString, Value: []byte(value), Actor: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtimepaths.BindCreated(options.UserData, dataDir); err != nil {
		t.Fatal(err)
	}
}

func modelReadiness(t *testing.T, options Options) string {
	t.Helper()
	for _, item := range run(t, options, "check-readiness", nil)["items"].([]map[string]string) {
		if item["id"] == "model" {
			return item["state"]
		}
	}
	t.Fatal("no model readiness")
	return ""
}

func TestReadinessAndModelProbeReadVaultSecrets(t *testing.T) {
	t.Setenv("DEER_FLOW_PORTABLE_ROOT", "")
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	options := setup(t)
	run(t, options, "init", nil)
	seedProfileSecrets(t, options, map[string]string{"MODEL_KEY": "vault-model-key", "SEARCH_KEY": "vault-search-key"})
	raw, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	saved := string(raw)
	if strings.Count(saved, "api_key: first-private-key") != 1 {
		t.Fatal("fixture model key not found")
	}
	for _, tc := range []struct{ key, want string }{
		{"${secret:MODEL_KEY}", "unverified"},
		{"${secret:ABSENT_KEY}", "missing_credential"},
		{"${secret:lower}", "missing_credential"},
	} {
		if err := os.WriteFile(options.Config, []byte(strings.Replace(saved, "first-private-key", tc.key, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := modelReadiness(t, options); got != tc.want {
			t.Errorf("readiness with %s = %s; want %s", tc.key, got, tc.want)
		}
	}

	model := modelByName(t, run(t, options, "snapshot", nil), "first")
	model["api_key"], model["base_url"] = "${secret:MODEL_KEY}", server.URL+"/v1"
	if result := run(t, options, "test-model", map[string]any{"model": model}); result["ok"] != true {
		t.Fatalf("probe with a vault key = %v", result)
	}
	model["api_key"] = "${secret:ABSENT_KEY}"
	if result := run(t, options, "test-model", map[string]any{"model": model}); result["ok"] != false || result["error_type"] != "MissingCredential" {
		t.Fatalf("probe with a missing vault key = %v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(keys, ",") != "Bearer vault-model-key" {
		t.Fatalf("model received %q", keys)
	}

	// web_search readiness must check whether a referenced secret actually exists.
	for _, name := range []string{"BRAVE_API_KEY", "DEEPSEEK_API_KEY", "SEARXNG_BASE_URL", "SEARXNG_API_KEY", "SEARXNG_TOKEN"} {
		t.Setenv(name, "")
	}
	if err := os.WriteFile(options.Config, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	doc["tools"] = append(list(doc, "tools"), map[string]any{"name": "web_search"})
	if err := writeYAMLAtomic(options.Config, doc); err != nil {
		t.Fatal(err)
	}
	searchPath := filepath.Join(filepath.Dir(options.Config), "search-providers.json")
	checkWebSearch := func(apiKey, want string) {
		t.Helper()
		payload := `{"version":1,"config":{"default_provider":"brave","fetch_provider":"local","providers":{"brave":{"api_key":"` + apiKey + `"}}}}`
		if err := os.WriteFile(searchPath, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		items := run(t, options, "check-readiness", nil)["items"].([]map[string]string)
		found := false
		for _, item := range items {
			if item["id"] == "web_search" {
				found = true
				if item["state"] != want {
					t.Fatalf("web_search readiness with %s = %+v; want %s", apiKey, item, want)
				}
			}
		}
		if !found {
			t.Fatalf("web_search missing from readiness items: %+v", items)
		}
	}
	checkWebSearch("${secret:ABSENT_SEARCH_KEY}", "missing_credential")
	checkWebSearch("${secret:SEARCH_KEY}", "unverified")
}
