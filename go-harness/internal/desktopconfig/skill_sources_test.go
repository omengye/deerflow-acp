package desktopconfig

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestManagedSourcesSaveLaunchAndRegistrySettingsStaySeparate(t *testing.T) {
	options, reviewDoc := reviewDocument(t)
	root := filepath.Join(options.UserData, "managed")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Disk content in a managed root is never an import installation.
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("invalid old disk content"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := readYAML(options.Config)
	raw["skills"] = map[string]any{"enabled": true, "managed_sources": []any{map[string]any{"id": "generated", "root": "../managed", "scope": "workspace", "workspace": ".."}}, "export_root": "../managed", "include_global": false}
	raw["skill_evolution"] = readObject(reviewDoc, "skill_evolution")
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	extPath := filepath.Join(filepath.Dir(options.Config), "extensions_config.json")
	extBytes := []byte("{\n  \"skills\": {\"old-skill\": {\"enabled\": false}},\n  \"other\": \"fixture-private-value\"\n}\n")
	if err := os.WriteFile(extPath, extBytes, 0600); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	if _, exists := doc["skills"]; exists {
		t.Fatal("snapshot still exposes obsolete filesystem skill state")
	}
	run(t, options, "validate", doc)
	doc = run(t, options, "save", doc)
	after, _ := os.ReadFile(extPath)
	if !bytes.Equal(after, extBytes) {
		t.Fatal("settings save rewrote import/extension state")
	}
	saved, _ := readYAML(options.Config)
	if !reflect.DeepEqual(saved["skills"], raw["skills"]) {
		t.Fatal("managed source configuration lost")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills.Sources) != 1 || cfg.Skills.Sources[0].Root != root || cfg.Skills.Sources[0].Workspace != options.UserData || cfg.Skills.ExportRoot != root || len(cfg.Skills.Install) != 0 || !reflect.DeepEqual(cfg.SkillProposalSources, []string{"generated"}) || cfg.SkillSelection.IncludeGlobal {
		t.Fatalf("managed YAML not consumed: %+v", cfg.Skills)
	}
	cfg.DataDir = filepath.Join(t.TempDir(), "runtime")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := deerflow.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	records, listErr := client.ListSkills(ctx, harness.SkillSelection{Workspace: options.UserData})
	closeErr := client.Close()
	if listErr != nil || closeErr != nil || len(records) != 0 {
		t.Fatal("managed directory became an imported skill", listErr, closeErr)
	}
	// Old clients must not change Registry enablement through legacy JSON.
	doc["skills"] = []any{map[string]any{"name": "old-skill", "enabled": true}}
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "skills") {
		t.Fatal("legacy skill write accepted", err)
	}
	after, _ = os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected legacy edit changed config")
	}
	after, _ = os.ReadFile(extPath)
	if !bytes.Equal(after, extBytes) {
		t.Fatal("rejected legacy edit changed import state")
	}
	delete(doc, "skills")
	readObject(doc, "skill_evolution")["enabled"] = false
	run(t, options, "save", doc)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || len(cfg.SkillProposalSources) != 0 || len(cfg.Skills.ManagedSources) != 1 {
		t.Fatal("evolution disable removed published source", err)
	}
}

func TestManagedSourceValidationAgreesAtSaveAndLaunch(t *testing.T) {
	for _, mode := range []string{"missing-export", "outside-workspace", "unknown-field", "duplicate", "reserved", "missing-root", "invalid-global", "not-array"} {
		t.Run(mode, func(t *testing.T) {
			options, _ := reviewDocument(t)
			root := filepath.Join(options.UserData, "managed")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			raw, _ := readYAML(options.Config)
			source := map[string]any{"id": "generated", "root": "../managed", "scope": "workspace", "workspace": ".."}
			settings := map[string]any{"enabled": true, "managed_sources": []any{source}, "export_root": "../managed"}
			switch mode {
			case "missing-export":
				delete(settings, "export_root")
			case "outside-workspace":
				source["workspace"] = "."
			case "unknown-field":
				source["api_key"] = "fixture-private-secret"
			case "duplicate":
				settings["managed_sources"] = []any{source, source}
			case "reserved":
				source["id"] = "python-custom"
			case "missing-root":
				source["root"] = "../missing"
			case "invalid-global":
				source["scope"] = "global"
			case "not-array":
				settings["managed_sources"] = source
			}
			raw["skills"] = settings
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(options.Config)
			snapshot, snapshotErr := Run(options, "snapshot", nil)
			var cfg deerflow.Config
			connections := 0
			_, launchErr := launch.ApplyConfig(options.Config, &cfg, &connections, nil)
			saveErr := snapshotErr
			if snapshotErr == nil {
				_, saveErr = Run(options, "save", mustJSON(t, snapshot))
			}
			for _, err := range []error{launchErr, saveErr} {
				if err == nil || (!strings.Contains(err.Error(), "skills.") && !strings.Contains(err.Error(), "/skill_sources")) || strings.Contains(err.Error(), "fixture-private-secret") {
					t.Fatal("invalid source accepted or private value leaked", err)
				}
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid source saved")
			}
		})
	}
}
