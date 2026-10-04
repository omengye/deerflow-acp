package desktopconfig

import (
	"os"
	"path/filepath"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

// Exercise the shipped templates through first-run initialization, settings save
// and daemon configuration. Old inert fields must not come back on first launch.
func TestNativeTemplatesInitializeSaveAndLaunch(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "resources/default-config.yaml", "resources/desktop-default-config.yaml", "desktop-app/resources/deerflow/default-config.yaml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			options := setup(t)
			if err := os.WriteFile(filepath.Join(options.Resources, "default-config.yaml"), data, 0600); err != nil {
				t.Fatal(err)
			}
			doc := run(t, options, "snapshot", nil)
			for _, key := range []string{"config_migration", "skill_evolution_migration"} {
				if readObject(doc, key)["required"] != false {
					t.Fatalf("fresh native template requires migration: %s", key)
				}
			}
			for round := 0; round < 2; round++ {
				raw, err := readYAML(options.Config)
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"api", "read_before_write", "tool_groups", "stream_bridge", "tracing", "llm_call", "tool_search", "tool_output", "guardrails"} {
					if _, ok := raw[key]; ok {
						t.Fatalf("inactive legacy section reintroduced: %s", key)
					}
				}
				for _, key := range []string{"checkpointer_path", "session_store_path", "goal_auto_continue", "goal_max_continuations", "agent_name", "thinking_enabled", "plan_mode", "recursion_limit"} {
					if _, ok := readObject(raw, "local_acp")[key]; ok {
						t.Fatalf("inactive runtime field reintroduced: %s", key)
					}
				}
				for _, key := range []string{"manager_class", "backend_config", "storage_class", "storage_path"} {
					if _, ok := readObject(raw, "memory")[key]; ok {
						t.Fatalf("legacy memory backend reintroduced: %s", key)
					}
				}
				for _, collection := range []string{"models", "tools"} {
					for _, value := range list(raw, collection) {
						for _, key := range []string{"use", "supports_thinking", "supports_reasoning_effort"} {
							if _, ok := value.(map[string]any)[key]; ok {
								t.Fatalf("inactive %s field in %s", key, collection)
							}
						}
					}
				}
				var cfg deerflow.Config
				connections := 0
				if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
					t.Fatal(err)
				}
				if cfg.Provider != "openai" || cfg.Model == "" || connections != 16 || !cfg.DisableSubagents || cfg.SkillGeneration != nil {
					t.Fatal("unexpected native model, opt-in or connection settings")
				}
				if round == 0 {
					doc = run(t, options, "save", doc)
				}
			}
		})
	}
}
