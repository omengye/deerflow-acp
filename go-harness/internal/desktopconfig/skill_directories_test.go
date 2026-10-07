package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestPortableManagedStoresInitializeForFreshAndCopiedConfig(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "copied"}[copied], func(t *testing.T) {
			options := setup(t)
			template := filepath.Join(options.Resources, "default-config.yaml")
			data, err := readYAML(template)
			if err != nil {
				t.Fatal(err)
			}
			settings := object(data, "skills")
			settings["managed_sources"] = []any{map[string]any{"id": "managed-evolution", "root": "../skills/evolved", "scope": "global"}}
			settings["export_root"] = "../skills-exports"
			if err := writeYAMLAtomic(template, data); err != nil {
				t.Fatal(err)
			}
			if copied {
				if err := writeYAMLAtomic(options.Config, data); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(options.Config)
			skill := filepath.Join(options.Resources, "skills", "public", "bootstrap", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(skill, []byte("---\nname: bootstrap\ndescription: fixture\n---\nFixture."), 0600); err != nil {
				t.Fatal(err)
			}
			doc := run(t, options, "snapshot", nil)
			if diagnostics, _ := doc["skill_source_diagnostics"].([]string); len(diagnostics) != 0 {
				t.Fatal(diagnostics)
			}
			for _, part := range []string{"skills/evolved", "skills-exports", "skills/public/bootstrap"} {
				info, err := os.Stat(filepath.Join(options.UserData, part))
				if err != nil || !info.IsDir() {
					t.Fatal("missing portable directory", part, err)
				}
			}
			marker := filepath.Join(options.UserData, "skills", "evolved", "preserve.txt")
			if err := os.WriteFile(marker, []byte("existing user data"), 0600); err != nil {
				t.Fatal(err)
			}
			run(t, options, "snapshot", nil)
			if body, err := os.ReadFile(marker); err != nil || string(body) != "existing user data" {
				t.Fatal("existing data changed", err)
			}
			after, _ := os.ReadFile(options.Config)
			if copied && !bytes.Equal(before, after) {
				t.Fatal("copied configuration was rewritten")
			}
			var cfg deerflow.Config
			connections := 32
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Skills.Install) != 1 {
				t.Fatal("store initialization suppressed bundled skills", len(cfg.Skills.Install))
			}
		})
	}
}

func TestMissingExternalStoreIsDiagnosedWithoutCreatingItOrBlockingRepair(t *testing.T) {
	options := setup(t)
	run(t, options, "snapshot", nil)
	data, _ := readYAML(options.Config)
	outside := filepath.Join(t.TempDir(), "missing")
	settings := object(data, "skills")
	settings["managed_sources"] = []any{map[string]any{"id": "custom", "root": outside, "scope": "global"}}
	settings["export_root"] = "../skills-exports"
	if err := writeYAMLAtomic(options.Config, data); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	diagnostics, _ := doc["skill_source_diagnostics"].([]string)
	if len(diagnostics) == 0 || !strings.Contains(strings.Join(diagnostics, " "), "skills.managed_sources.0") {
		t.Fatal("missing directory not diagnosed", diagnostics)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("external directory was created", err)
	}
	doc["skills_enabled"] = false
	run(t, options, "save", doc)
}

func TestPortableStoreObstructionKeepsSnapshotAvailableAndDoesNotFollowLinks(t *testing.T) {
	for _, link := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "link"}[link], func(t *testing.T) {
			options := setup(t)
			run(t, options, "snapshot", nil)
			path := filepath.Join(options.UserData, "skills", "evolved")
			outside := t.TempDir()
			if link {
				if err := os.Symlink(outside, path); err != nil {
					t.Skip("symlink unavailable", err)
				}
			} else if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			data, _ := readYAML(options.Config)
			settings := object(data, "skills")
			settings["managed_sources"] = []any{map[string]any{"id": "managed-evolution", "root": "../skills/evolved/new", "scope": "global"}}
			settings["export_root"] = "../skills-exports"
			if err := writeYAMLAtomic(options.Config, data); err != nil {
				t.Fatal(err)
			}
			doc := run(t, options, "snapshot", nil)
			if diagnostics, _ := doc["skill_source_diagnostics"].([]string); len(diagnostics) == 0 {
				t.Fatal("obstruction not diagnosed")
			}
			if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
				t.Fatal("write escaped through link", err)
			}
		})
	}
}
