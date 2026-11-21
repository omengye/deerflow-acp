package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func isDesktopTemplate(name string) bool {
	return name == "resources/desktop-default-config.yaml" || name == "desktop-app/resources/deerflow/default-config.yaml"
}

// Exercise the shipped templates through first-run initialization, settings save
// and daemon configuration. Old inert fields must not come back on first launch.
func TestNativeTemplatesInitializeSaveAndLaunch(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "resources/default-config.yaml", "resources/desktop-default-config.yaml", "desktop-app/resources/deerflow/default-config.yaml"} {
		t.Run(name, func(t *testing.T) {
			desktop := isDesktopTemplate(name)
			data, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			options := setup(t)
			if err := os.WriteFile(filepath.Join(options.Resources, "default-config.yaml"), data, 0600); err != nil {
				t.Fatal(err)
			}
			doc := run(t, options, "snapshot", nil)
			for _, key := range []string{"config_migration"} {
				if readObject(doc, key)["required"] != false {
					t.Fatalf("fresh native template requires migration: %s", key)
				}
			}
			applyConfig := func() (deerflow.Config, int, error) {
				var cfg deerflow.Config
				var explicit map[string]bool
				if desktop && runtime.GOOS != "windows" {
					// Non-Windows hosts need an explicit provider for desktop templates.
					// This test loads the configuration without executing a shell.
					cfg.Sandbox = harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true}
					explicit = map[string]bool{"sandbox-provider": true}
				}
				connections := 0
				_, err := launch.ApplyConfig(options.Config, &cfg, &connections, explicit)
				return cfg, connections, err
			}
			for round := 0; round < 2; round++ {
				for _, key := range []string{"tool_diagnostics", "skill_source_diagnostics"} {
					if diagnostics := doc[key].([]string); len(diagnostics) != 0 {
						t.Fatalf("fresh template has %s: %v", key, diagnostics)
					}
				}
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
						for _, key := range []string{"use", "group", "supports_thinking", "supports_reasoning_effort"} {
							if _, ok := value.(map[string]any)[key]; ok {
								t.Fatalf("inactive %s field in %s", key, collection)
							}
						}
					}
				}
				cfg, connections, err := applyConfig()
				if err != nil {
					t.Fatal(err)
				}
				wantConnections := 16
				if desktop {
					wantConnections = 15
					if !cfg.Sandbox.Enabled || !cfg.Sandbox.AllowShell || !cfg.HostToolsAllowed || cfg.MemoryScope != harness.MemoryScope("global") || cfg.SkillLearning == nil || cfg.SkillDiscovery == nil {
						t.Fatal("desktop capabilities require extra non-model configuration")
					}
					if cfg.SkillGeneration == nil || cfg.SkillGeneration.Model.Route.Model != cfg.Model {
						t.Fatal("desktop generation does not inherit the configured model")
					}
					if !cfg.SkillGeneration.PublishAfterSecurityReview || cfg.SkillBehavior != nil || cfg.SkillBehaviorEvaluator != nil || cfg.SkillAutoPatch != nil {
						t.Fatal("native publication must use only the security reviewer")
					}
					wantBudget := harness.SkillMaintenancePolicy{MaxJobSeconds: 900, Workers: 2}
					if cfg.SkillGeneration.Policy != wantBudget || cfg.SkillLearning.Policy != wantBudget {
						t.Fatal("desktop maintenance budgets differ from the selected defaults")
					}
					if !cfg.SkillLearning.CrossSession || cfg.SkillDiscovery.CooldownHours != 24 {
						t.Fatal("desktop defaults changed source-sharing authority or publication bounds")
					}
					for _, part := range []string{"skills/evolved", "skills-exports"} {
						if info, err := os.Stat(filepath.Join(options.UserData, part)); err != nil || !info.IsDir() {
							t.Fatalf("desktop store not initialized: %s: %v", part, err)
						}
					}
				}
				if cfg.Provider != "openai" || cfg.Model == "" || connections != wantConnections || cfg.DisableSubagents == desktop || cfg.SkillGeneration == nil {
					t.Fatal("unexpected native model, opt-in or connection settings")
				}
				if round == 1 && (cfg.Model != "fixture-model" || cfg.APIKey != "fixture-model-key" || cfg.BaseURL != "https://model.example.test/v1") {
					t.Fatal("model-only setup was not consumed")
				}
				if round == 0 {
					model := modelByName(t, doc, str(doc, "default_model", ""))
					model["model"], model["api_key"], model["base_url"] = "fixture-model", "fixture-model-key", "https://model.example.test/v1"
					doc = run(t, options, "save", doc)
				}
			}
			if desktop {
				// A retired suite must not be required, parsed or overwritten.
				path := filepath.Join(filepath.Dir(options.Config), "skill-behavior", "old.json")
				if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatal("unexpected behavior registry", err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("invalid historical suite"), 0600); err != nil {
					t.Fatal(err)
				}
				run(t, options, "init", nil)
				if _, _, err := applyConfig(); err != nil {
					t.Fatal("historical suite blocked native startup", err)
				}
				if content, err := os.ReadFile(path); err != nil || string(content) != "invalid historical suite" {
					t.Fatal("historical suite was modified", err)
				}
			}
		})
	}
}

func TestNewDesktopDefaultsDoNotOverwriteExistingConfig(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "resources", "desktop-default-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.Resources, "default-config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	run(t, options, "snapshot", nil)
	after, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("new desktop defaults overwrote the existing user configuration")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(options.Config), "skill-behavior")); !os.IsNotExist(err) {
		t.Fatal("native initialization created a behavior registry", err)
	}
}

func TestDesktopTemplatesMatchAndContainNoPersonalSettings(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	packaged, err := os.ReadFile(filepath.Join(root, "resources", "desktop-default-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile(filepath.Join(root, "desktop-app", "resources", "deerflow", "default-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packaged, embedded) {
		t.Fatal("packaged and embedded desktop defaults differ")
	}
	raw, err := readYAML(filepath.Join(root, "resources", "desktop-default-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	models := list(raw, "models")
	if len(models) != 1 || models[0].(map[string]any)["api_key"] != "${OPENAI_API_KEY:-not-configured}" {
		t.Fatal("desktop template must contain only a placeholder model credential")
	}
	for _, item := range list(raw, "tools") {
		tool := item.(map[string]any)
		for _, key := range []string{"api_key", "https_proxy", "group"} {
			if _, exists := tool[key]; exists {
				t.Fatalf("desktop template contains personal or inert tool field %s", key)
			}
		}
	}
	if toolByName(t, raw, "host_opencli")["executable"] != "opencli" || len(readObject(readObject(raw, "subagents"), "custom_agents")) != 0 {
		t.Fatal("desktop template contains machine-specific launcher or custom roles")
	}
}
