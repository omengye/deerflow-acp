package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/runtimepaths"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func seedConfigSecrets(t *testing.T, dataDir string, values map[string][]byte) {
	t.Helper()
	ctx := context.Background()
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
		kind := secrets.KindString
		if !utf8.Valid(value) {
			kind = secrets.KindFile
		}
		if _, err := vault.Set(ctx, secrets.SetRequest{Name: name, Kind: kind, Value: value, Actor: "test"}); err != nil {
			t.Fatal(err)
		}
	}
}

func secretConfig(root, selectedKey string) string {
	return fmt.Sprintf(`default_model: selected
models:
  - name: selected
    use: langchain_openai:ChatOpenAI
    model: selected-id
    api_key: %s
    base_url: https://selected.example/v1
  - name: routed
    use: langchain_openai:ChatOpenAI
    model: routed-id
    api_key: ${secret:ROUTE_KEY}
    base_url: https://routed.example/v1
  - name: unstored
    use: langchain_openai:ChatOpenAI
    model: unstored-id
    api_key: ${secret:NOT_STORED}
    base_url: https://unstored.example/v1
tools:
  - name: web_search
    api_key: ${secret:WEB_SEARCH_KEY}
network:
  proxy:
    enabled: true
    url: ${secret:PROXY_URL}
mcp:
  host_recipes:
    - id: fixture
      workspace: %s
      servers:
        - name: remote
          transport: http
          url: https://mcp.example/mcp
          headers:
            Authorization: ${secret:MCP_HEADER}
tracing:
  langfuse:
    secret_key: ${secret:IGNORED_BY_RUNTIME}
`, selectedKey, root)
}

func TestApplyConfigResolvesSecretReferences(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	path := filepath.Join(root, "config.yaml")
	// The default data directory of a config outside a portable profile.
	seedConfigSecrets(t, filepath.Join(root, "go-harness-state"), map[string][]byte{
		"MODEL_KEY":      []byte("model-secret-value-1"),
		"ROUTE_KEY":      []byte("route-secret-value-2"),
		"WEB_SEARCH_KEY": []byte("tool-secret-value-3"),
		"PROXY_URL":      []byte("http://user:pass@proxy.example:8080"),
		"MCP_HEADER":     []byte("Bearer mcp-secret-value-4"),
		"BINARY":         {0xff, 0x00, 0xfe},
	})
	if err := os.WriteFile(path, []byte(secretConfig(root, "${secret:MODEL_KEY}")), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != path || cfg.APIKey != "model-secret-value-1" || cfg.ModelRoutes["deerflow-config:routed"].APIKey != "route-secret-value-2" {
		t.Fatalf("model credentials: path=%q key=%q routes=%v", cfg.ConfigPath, cfg.APIKey, len(cfg.ModelRoutes))
	}
	if _, ok := cfg.ModelRoutes["deerflow-config:unstored"]; ok {
		t.Fatal("a model whose secret is missing was routed")
	}
	if len(cfg.BuiltinTools) != 1 || cfg.BuiltinTools[0].APIKey != "tool-secret-value-3" {
		t.Fatal("builtin tool credential was not resolved")
	}
	// Search resolves its credentials for each request, so it keeps the reference.
	if cfg.Search.Providers["brave"].APIKey != "${secret:WEB_SEARCH_KEY}" {
		t.Fatalf("search provider key = %q", cfg.Search.Providers["brave"].APIKey)
	}
	if cfg.Network.Proxy.URL != "http://user:pass@proxy.example:8080" {
		t.Fatal("proxy reference was not resolved")
	}
	if len(cfg.HostMCPRecipes) != 1 || cfg.HostMCPRecipes[0].Servers[0].Headers["Authorization"] != "Bearer mcp-secret-value-4" {
		t.Fatal("MCP header reference was not resolved")
	}

	for _, tc := range []struct{ key, want string }{
		{"${secret:NOT_STORED}", "secret NOT_STORED required by config is missing from the secret vault"},
		{"${secret:BINARY}", "secret BINARY required by config is binary"},
		{"${secret:lower}", "invalid secret reference"},
	} {
		if err := os.WriteFile(path, []byte(secretConfig(root, tc.key)), 0o600); err != nil {
			t.Fatal(err)
		}
		var failed deerflow.Config
		if _, err := ApplyConfig(path, &failed, &connections, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("selected key %s: %v; want %q", tc.key, err, tc.want)
		}
	}
}

func TestApplyConfigReadsSecretsWithoutChangingTheDataDirectory(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(secretConfig(root, "${secret:MODEL_KEY}")), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(root, "absent-state")
	cfg := deerflow.Config{DataDir: absent}
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, map[string]bool{"data-dir": true}); err == nil || !strings.Contains(err.Error(), "proxy reference could not be resolved: secret PROXY_URL required by config is missing from the secret vault") {
		t.Fatalf("missing vault = %v", err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading secrets created the data directory: %v", err)
	}

	// A vault that cannot be read fails only the settings that use it.
	broken := filepath.Join(root, "broken-state")
	seedConfigSecrets(t, broken, map[string][]byte{"MODEL_KEY": []byte("model-secret-value-1")})
	if err := os.WriteFile(filepath.Join(runtimepaths.SecretVault(broken), "master.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = deerflow.Config{DataDir: broken}
	if _, err := ApplyConfig(path, &cfg, &connections, map[string]bool{"data-dir": true}); err == nil || !strings.Contains(err.Error(), "cannot be read from the secret vault") {
		t.Fatalf("unreadable vault = %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(secretConfig(root, "plain-model-key"), "url: ${secret:PROXY_URL}", "url: http://proxy.example:8080", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Only the routed and MCP settings still refer to the vault now; the
	// unreadable vault skips the routes and fails the MCP header.
	cfg = deerflow.Config{DataDir: broken}
	if _, err := ApplyConfig(path, &cfg, &connections, map[string]bool{"data-dir": true}); err == nil || !strings.Contains(err.Error(), "header credential reference is unavailable") {
		t.Fatalf("unreadable vault for an MCP header = %v", err)
	}
}

// A second launch reads the vault while a runtime holds the same data directory.
func TestApplyConfigReadsSecretsWhileTheRuntimeRuns(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	path := filepath.Join(root, "config.yaml")
	dataDir := filepath.Join(root, "go-harness-state")
	seedConfigSecrets(t, dataDir, map[string][]byte{"MODEL_KEY": []byte("model-secret-value-1")})
	client, err := deerflow.Open(context.Background(), deerflow.Config{DataDir: dataDir, Engine: retentionEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw := "default_model: selected\nmodels:\n  - name: selected\n    use: langchain_openai:ChatOpenAI\n    model: selected-id\n    api_key: ${secret:MODEL_KEY}\n    base_url: https://selected.example/v1\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil || cfg.APIKey != "model-secret-value-1" {
		t.Fatalf("ApplyConfig while running: key resolved=%v err=%v", cfg.APIKey == "model-secret-value-1", err)
	}
}
