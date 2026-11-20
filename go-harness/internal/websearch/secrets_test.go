package websearch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// A ${secret:NAME} api_key is resolved for each request. Settings, pinned
// execution versions and status keep the reference, never the value.
func TestSecretAPIKeysResolvePerRequest(t *testing.T) {
	isolatedEnvironment(t)
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Subscription-Token")+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/tools/fetch" {
			fmt.Fprint(w, `{"title":"Page","content":"# Markdown"}`)
			return
		}
		fmt.Fprint(w, `{"web":{"results":[{"title":"Brave","url":"https://example.test/brave","description":"snippet"}]}}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	m := mustManager(t, filepath.Join(dir, "search-providers.json"), harness.SearchConfig{DefaultProvider: "brave", FetchProvider: "deepseek", Providers: map[string]harness.SearchProviderConfig{
		"brave":    {BaseURL: server.URL, APIKey: "${secret:BRAVE_KEY}"},
		"deepseek": {BaseURL: server.URL, APIKey: "${secret:DEEPSEEK_KEY}"},
	}})
	status, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range status.Providers {
		if provider.Name == "brave" && (!provider.Configured || !provider.APIKeyConfigured) {
			t.Fatalf("a secret reference does not configure the provider: %+v", provider)
		}
	}
	snapshot, err := m.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := snapshot.Search(ctx, Request{Provider: "brave", Query: "q"}); err == nil || !strings.Contains(err.Error(), "no secret vault is open") {
		t.Fatalf("search without a vault = %v", err)
	}

	values := map[string]string{"BRAVE_KEY": "brave-secret-value-1"}
	m.UseSecrets(func(_ context.Context, name string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		value, ok := values[name]
		if !ok {
			return "", fmt.Errorf("secret %s is not stored", name)
		}
		return value, nil
	})
	if _, err := snapshot.Search(ctx, Request{Provider: "brave", Query: "q"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	values["BRAVE_KEY"] = "brave-secret-value-2"
	mu.Unlock()
	// A rotated secret applies to the next request of the same snapshot.
	if _, err := snapshot.Search(ctx, Request{Provider: "brave", Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Fetch(ctx, "https://example.test/page"); err == nil || !strings.Contains(err.Error(), "DEEPSEEK_KEY is not stored") {
		t.Fatalf("fetch with a missing secret = %v", err)
	}
	mu.Lock()
	values["DEEPSEEK_KEY"] = "injected\r\nX-Other: header"
	mu.Unlock()
	if _, err := snapshot.Fetch(ctx, "https://example.test/page"); err == nil || !strings.Contains(err.Error(), "not a usable search credential") {
		t.Fatalf("fetch with an unusable secret = %v", err)
	}
	mu.Lock()
	values["DEEPSEEK_KEY"] = "deepseek-secret-value"
	mu.Unlock()
	if content, err := snapshot.Fetch(ctx, "https://example.test/page"); err != nil || content != "# Markdown" {
		t.Fatalf("fetch = %q, %v", content, err)
	}
	mu.Lock()
	got := strings.Join(seen, ",")
	mu.Unlock()
	if got != "brave-secret-value-1,brave-secret-value-2,Bearer deepseek-secret-value" {
		t.Fatalf("providers received %q", got)
	}

	// Settings and pinned versions on disk hold the references only.
	pinned := 0
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "secret-value") {
			t.Errorf("%s holds a secret value", path)
		}
		if strings.Contains(string(body), "${secret:BRAVE_KEY}") {
			pinned++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if pinned == 0 {
		t.Fatal("no pinned version kept the reference")
	}
}

func TestSecretReferencesInSearchSettings(t *testing.T) {
	isolatedEnvironment(t)
	for _, providers := range []map[string]harness.SearchProviderConfig{
		{"searxng": {BaseURL: "${secret:SEARXNG_URL}"}},
		{"brave": {APIKey: "key", HTTPSProxy: "${secret:PROXY}"}},
		{"brave": {APIKey: "${secret:lower}"}},
	} {
		if err := Validate(harness.SearchConfig{Providers: providers}); err == nil {
			t.Errorf("Validate(%+v) accepted a secret reference it cannot resolve", providers)
		}
	}
	if err := Validate(harness.SearchConfig{Providers: map[string]harness.SearchProviderConfig{"brave": {APIKey: "${secret:BRAVE_KEY}"}}}); err != nil {
		t.Fatalf("api_key reference rejected: %v", err)
	}
	// The official DeepSeek model credential is reused by reference.
	config, err := SeedYAML([]byte("models:\n  - name: deepseek\n    base_url: https://api.deepseek.com\n    api_key: ${secret:MODEL_DEEPSEEK_API_KEY}\ntools:\n  - name: web_search\n    api_key: ${secret:WEB_SEARCH_API_KEY}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Providers["deepseek"].APIKey != "${secret:MODEL_DEEPSEEK_API_KEY}" || config.Providers["brave"].APIKey != "${secret:WEB_SEARCH_API_KEY}" {
		t.Fatalf("seeded providers = %+v", config.Providers)
	}
}

func TestReferenceKeysReplacesOnlySavedPlaintext(t *testing.T) {
	isolatedEnvironment(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "search-providers.json")
	// The seed alone is config.yaml's business; nothing is saved yet.
	m := mustManager(t, path, harness.SearchConfig{Providers: map[string]harness.SearchProviderConfig{"brave": {APIKey: "seed-plaintext-key"}}})
	if keys, err := m.StoredPlaintextKeys(); err != nil || len(keys) != 0 {
		t.Fatalf("seed-only keys = %v, %v", keys, err)
	}
	if changed, err := m.ReferenceKeys(ctx, map[string]KeyReference{"brave": {Value: "seed-plaintext-key", Secret: "BRAVE"}}); err != nil || len(changed) != 0 {
		t.Fatalf("seed-only reference = %v, %v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("referencing seed keys saved settings: %v", err)
	}

	status, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	brave, deepseek, searx := "brave-plaintext-key", "deepseek-plaintext-key", "${secret:ALREADY}"
	for provider, key := range map[string]*string{"brave": &brave, "deepseek": &deepseek, "searxng": &searx} {
		if status, err = m.Update(ctx, Update{Provider: provider, ExpectedRevision: status.Revision, APIKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := m.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.verified(ctx, "brave")
	if pinned, err := m.PinnedPlaintextVersions(); err != nil || pinned != 2 {
		t.Fatalf("pinned plaintext versions = %d, %v", pinned, err)
	}
	keys, err := m.StoredPlaintextKeys()
	if err != nil || len(keys) != 2 || keys["brave"] != brave || keys["deepseek"] != deepseek {
		t.Fatalf("stored keys = %v, %v", len(keys), err)
	}
	if _, err := m.ReferenceKeys(ctx, map[string]KeyReference{"brave": {Value: brave, Secret: "lower"}}); err == nil {
		t.Fatal("invalid secret name accepted")
	}
	// deepseek changed after it was read, so it stays as it is.
	changed, err := m.ReferenceKeys(ctx, map[string]KeyReference{"brave": {Value: brave, Secret: "BRAVE_KEY"}, "deepseek": {Value: "an-older-value", Secret: "DEEPSEEK_KEY"}})
	if err != nil || strings.Join(changed, ",") != "brave" {
		t.Fatalf("changed = %v, %v", changed, err)
	}
	status, err = m.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range status.Providers {
		if provider.Name == "brave" && provider.State != "verified" {
			t.Fatalf("brave lost its verification: %s", provider.State)
		}
	}
	refs, err := m.SecretReferences()
	if err != nil || strings.Join(refs["BRAVE_KEY"], ",") != "brave" || strings.Join(refs["ALREADY"], ",") != "searxng" || len(refs) != 2 {
		t.Fatalf("references = %v, %v", refs, err)
	}
	body, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(body), brave) || !strings.Contains(string(body), deepseek) {
		t.Fatalf("saved settings = %s, %v", body, err)
	}

	// An unset environment reference falls back to the seed's key, which
	// config.yaml holds; the settings file keeps its own reference.
	t.Setenv("UNSET_BRAVE_KEY", "")
	envRef := "${UNSET_BRAVE_KEY:-}"
	if status, err = m.Status(); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Update(ctx, Update{Provider: "brave", ExpectedRevision: status.Revision, APIKey: &envRef}); err != nil {
		t.Fatal(err)
	}
	if changed, err := m.ReferenceKeys(ctx, map[string]KeyReference{"brave": {Value: "seed-plaintext-key", Secret: "SEED_KEY"}}); err != nil || len(changed) != 0 {
		t.Fatalf("seed fallback reference = %v, %v", changed, err)
	}
	if body, err := os.ReadFile(path); err != nil || !strings.Contains(string(body), envRef) || strings.Contains(string(body), "SEED_KEY") {
		t.Fatalf("saved settings after a seed fallback = %s, %v", body, err)
	}
}

func TestSecretCheckDetectsMissingSecretInStatusAndAutoSelection(t *testing.T) {
	isolatedEnvironment(t)
	dir := t.TempDir()
	m := mustManager(t, filepath.Join(dir, "search-providers.json"), harness.SearchConfig{
		DefaultProvider: "auto",
		Providers: map[string]harness.SearchProviderConfig{
			"brave":    {APIKey: "${secret:DELETED_BRAVE_KEY}"},
			"deepseek": {APIKey: "${secret:LIVE_DEEPSEEK_KEY}"},
		},
	})
	m.UseSecretCheck(func(_ context.Context, name string) bool {
		return name == "LIVE_DEEPSEEK_KEY"
	})
	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.DefaultProvider != "deepseek" {
		t.Fatalf("auto default provider = %q; want deepseek when brave secret is absent", st.DefaultProvider)
	}
	for _, p := range st.Providers {
		if p.Name == "brave" && (p.Configured || p.APIKeyConfigured || p.State != "not_configured") {
			t.Fatalf("missing secret brave status = %+v", p)
		}
		if p.Name == "deepseek" && (!p.Configured || !p.APIKeyConfigured) {
			t.Fatalf("present secret deepseek status = %+v", p)
		}
	}
}
