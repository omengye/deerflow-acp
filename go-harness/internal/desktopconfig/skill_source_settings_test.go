package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func sourceSettingsFixture(t *testing.T) (Options, map[string]any) {
	t.Helper()
	options, _ := reviewDocument(t)
	for _, part := range []string{"managed", "exports"} {
		if err := os.Mkdir(filepath.Join(options.UserData, part), 0700); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := readYAML(options.Config)
	readObject(raw, "skills")["unused_legacy_field"] = map[string]any{"credential": "fixture-source-secret"}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	document := run(t, options, "snapshot", nil)
	sources := readObject(document, "skill_sources")
	sources["import_path"] = ""
	sources["export_root"] = "../exports"
	sources["managed"] = []any{map[string]any{"id": "generated", "root": "../managed", "scope": "workspace", "workspace": ".."}}
	return options, document
}

func TestSkillSourceFormSaveUsesActualPathsScopesAndGlobalSelection(t *testing.T) {
	options, doc := sourceSettingsFixture(t)
	if bytes.Contains(mustJSON(t, doc), []byte("fixture-source-secret")) {
		t.Fatal("source diagnostics leaked original data")
	}
	if !reflect.DeepEqual(doc["skill_source_diagnostics"], []string{"skills.unused_legacy_field"}) {
		t.Fatal("missing source migration diagnostics")
	}
	readObject(doc, "skill_evolution")["enabled"] = true
	readObject(doc, "skill_evolution")["moderation_model_name"] = "first"
	readObject(doc, "skill_evolution")["evaluation_model_name"] = "second"
	doc = run(t, options, "save", doc)
	raw, _ := readYAML(options.Config)
	settings := readObject(raw, "skills")
	if _, found := settings["path"]; found {
		t.Fatal("cleared import path still shadows managed selection")
	}
	if readObject(settings, "unused_legacy_field")["credential"] != "fixture-source-secret" {
		t.Fatal("unrelated legacy data lost")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills.Sources) != 1 || cfg.Skills.Sources[0].Scope != harness.SkillScopeWorkspace || cfg.Skills.Sources[0].Workspace != options.UserData || cfg.Skills.ExportRoot != filepath.Join(options.UserData, "exports") || cfg.Skills.Sources[0].Root != filepath.Join(options.UserData, "managed") || cfg.SkillSelection.IncludeGlobal || !reflect.DeepEqual(cfg.SkillProposalSources, []string{"generated"}) {
		t.Fatal("saved source policy not consumed")
	}
	// Scope changes and explicit global opt-in must reach the next startup.
	sources := readObject(doc, "skill_sources")
	entry := list(sources, "managed")[0].(map[string]any)
	entry["scope"], entry["workspace"] = "global", ""
	sources["include_global"] = true
	doc = run(t, options, "save", doc)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || !cfg.SkillSelection.IncludeGlobal || cfg.Skills.Sources[0].Scope != harness.SkillScopeGlobal || cfg.Skills.Sources[0].Workspace != "" {
		t.Fatal("global scope edit not consumed", err)
	}
	// Clearing the override restores the managed-only default, not true.
	readObject(doc, "skill_sources")["include_global"] = nil
	doc = run(t, options, "save", doc)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillSelection.IncludeGlobal {
		t.Fatal("inheritance did not restore native global policy", err)
	}
	readObject(doc, "skill_evolution")["enabled"] = false
	doc["skills_enabled"] = false
	readObject(doc, "skill_sources")["managed"] = []any{}
	readObject(doc, "skill_sources")["export_root"] = ""
	run(t, options, "save", doc)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || len(cfg.Skills.Sources) != 0 || cfg.SkillSelection.Names == nil {
		t.Fatal("disabled source removal ineffective", err)
	}
	if info, err := os.Stat(filepath.Join(options.UserData, "managed")); err != nil || !info.IsDir() {
		t.Fatal("configuration removal deleted source directory", err)
	}
}

func TestSkillSourceFormRejectsInvalidEditsWithoutWrites(t *testing.T) {
	for _, mode := range []string{"unknown", "missing-field", "duplicate", "reserved", "bad-name", "global-workspace", "outside-workspace", "missing-root", "missing-export", "wrong-type", "disabled-new-path"} {
		t.Run(mode, func(t *testing.T) {
			options, doc := sourceSettingsFixture(t)
			sources := readObject(doc, "skill_sources")
			entry := list(sources, "managed")[0].(map[string]any)
			switch mode {
			case "unknown":
				entry["api_key"] = "fixture-source-secret"
			case "missing-field":
				delete(entry, "scope")
			case "duplicate":
				sources["managed"] = []any{entry, entry}
			case "reserved":
				entry["id"] = "python-public"
			case "bad-name":
				entry["id"] = "../escape"
			case "global-workspace":
				entry["scope"] = "global"
			case "outside-workspace":
				entry["workspace"] = "."
			case "missing-root":
				entry["root"] = "../missing"
			case "missing-export":
				sources["export_root"] = ""
			case "wrong-type":
				sources["include_global"] = "true"
			case "disabled-new-path":
				doc["skills_enabled"] = false
				entry["root"] = "../missing"
			}
			before, _ := os.ReadFile(options.Config)
			for _, operation := range []string{"validate", "save"} {
				if _, err := Run(options, operation, mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "skill") || strings.Contains(err.Error(), "fixture-source-secret") {
					t.Fatal("invalid source edit accepted or input leaked", err)
				}
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid source edit modified config")
			}
		})
	}
}

func TestDisabledUnchangedSourceConfigurationSurvivesUnrelatedSave(t *testing.T) {
	options, _ := reviewDocument(t)
	raw, _ := readYAML(options.Config)
	settings := readObject(raw, "skills")
	settings["enabled"] = false
	settings["managed_sources"] = []any{map[string]any{"id": "old-invalid-id--", "root": "../missing", "scope": "global"}}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	readObject(doc, "runtime")["max_active_runs"] = 4
	doc = run(t, options, "save", doc)
	saved, _ := readYAML(options.Config)
	if !reflect.DeepEqual(readObject(saved, "skills"), settings) {
		t.Fatal("dormant original source data changed")
	}
	doc["skills_enabled"] = true
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("invalid dormant source enabled")
	}
}
