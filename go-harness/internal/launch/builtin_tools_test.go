package launch

import (
	"os"
	"path/filepath"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
)

func TestNativeToolConfigurationWithoutPythonUses(t *testing.T) {
	t.Setenv("FIXTURE_SEARCH_KEY", "fixture-secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `models:
  - name: primary
    provider: openai
    model: fixture-id
  - name: alternate
    provider: claude
    model: fixture-other
default_model: primary
sandbox:
  provider: local
  allow_host_tools: true
tools:
  - name: web_search
    api_key: $FIXTURE_SEARCH_KEY
    https_proxy: http://127.0.0.1:11808
    max_results: 5
  - name: web_fetch
    timeout: 10
  - name: image_search
    max_results: 5
  - name: host_opencli
    executable: ./opencli.exe
    allowed_sites: [web, github]
  - name: glob
    max_results: 200
  - name: grep
    max_results: 100
  - name: move_path
  - name: delete_path
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyPythonConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openai" || cfg.ModelRoutes["deerflow-config:alternate"].Provider != "claude" || !cfg.HostToolsAllowed || len(cfg.BuiltinTools) != 8 {
		t.Fatal("native settings not consumed")
	}
	if cfg.BuiltinTools[0].APIKey != "fixture-secret" || cfg.BuiltinTools[0].MaxResults != 5 || cfg.BuiltinTools[1].Timeout != 10 || cfg.BuiltinTools[3].Executable != filepath.Join(filepath.Dir(path), "opencli.exe") {
		t.Fatal("tool settings lost")
	}
}
