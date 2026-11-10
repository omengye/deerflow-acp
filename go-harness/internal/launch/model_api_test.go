package launch

import (
	"os"
	"path/filepath"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestApplyModelAPIRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `default_model: first
models:
  - name: first
    provider: openai
    model: shared-id
    base_url: https://fixture.example/v1
    api: responses
  - name: second
    provider: openai
    model: shared-id
    base_url: https://fixture.example/v1
    api: chat_completions
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.API != harness.ModelAPIResponses || cfg.ModelRoutes["deerflow-config:second"].API != "" {
		t.Fatal("model protocols were lost or merged across distinct configured routes")
	}
	overridden := deerflow.Config{BaseURL: "https://other.example/v1"}
	if _, err := ApplyConfig(path, &overridden, &connections, map[string]bool{"base-url": true}); err != nil {
		t.Fatal(err)
	}
	if overridden.API != "" {
		t.Fatal("selected model protocol crossed a CLI endpoint override")
	}
	var pinned deerflow.Config
	if _, err := ApplyConfig(path, &pinned, &connections, map[string]bool{"api-key": true}); err != nil {
		t.Fatal(err)
	}
	if len(pinned.Models) != 1 || pinned.Models[0].Name != "first" {
		t.Fatal("a pinned backend advertised a model requiring another protocol")
	}
}
