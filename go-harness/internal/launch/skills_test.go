package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillsConfigurationManagedSourcesAndReviewGrants(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "skills.json")
	document := map[string]any{
		"sources":        []harness.SkillSource{{ID: "generated", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: root}},
		"managedSources": []string{"generated"}, "exportRoot": root,
	}
	data, _ := json.Marshal(document)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseRuntimeFlags(t, "--skills-config", path)
	if err != nil || len(cfg.Skills.ManagedSources) != 1 || cfg.Skills.ManagedSources[0] != "generated" || cfg.Skills.ExportRoot != root || len(cfg.Skills.Install) != 0 {
		t.Fatalf("managed source flags: %+v %v", cfg.Skills, err)
	}
	yamlPath := writeRoleConfig(t, roleModels+"skill_evolution:\n  enabled: true\n  mode: review\n  moderation_model_name: main\n  evaluation_model_name: research\n")
	connections := 0
	if _, err = ApplyConfig(yamlPath, &cfg, &connections, map[string]bool{"skills-config": true}); err != nil || len(cfg.SkillProposalSources) != 1 || cfg.SkillProposalSources[0] != "generated" || cfg.Skills.ExportRoot != root {
		t.Fatalf("managed source review grant: %+v %v", cfg.SkillProposalSources, err)
	}
	// Disabling evolution revokes generation, not published managed skills.
	yamlPath = writeRoleConfig(t, roleModels+"skill_evolution:\n  enabled: false\n")
	if _, err = ApplyConfig(yamlPath, &cfg, &connections, map[string]bool{"skills-config": true}); err != nil || len(cfg.SkillProposalSources) != 0 || len(cfg.Skills.ManagedSources) != 1 {
		t.Fatalf("evolution disable removed managed source: %+v %v", cfg.Skills, err)
	}
}

func TestSkillsConfigurationHasExplicitSourceAndInstallSelection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "skills.json")
	document := map[string]any{"sources": []harness.SkillSource{{ID: "workspace", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: root}}, "install": []harness.SkillInstall{{SourceID: "workspace", Directory: "research", Enabled: true}}, "includeGlobal": true, "names": []string{"research"}}
	data, _ := json.Marshal(document)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseRuntimeFlags(t, "--skills-config", path, "--vision-model", "vision-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills.Sources) != 1 || cfg.Skills.Sources[0].Root != root || len(cfg.Skills.Install) != 1 || !cfg.Skills.Install[0].Enabled || !cfg.SkillSelection.IncludeGlobal || len(cfg.SkillSelection.Names) != 1 || !cfg.Media.SupportsVision("vision-test") {
		t.Fatalf("flags=%+v", cfg)
	}
	if cfg.SkillSelection.Workspace != "" {
		t.Fatal("runtime must derive workspace from session")
	}
	for _, bad := range []string{"null", `{"sources":[],"unknown":true}`, `{} {}`, `{"names":false}`} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := parseRuntimeFlags(t, "--skills-config", path); err == nil {
			t.Fatalf("invalid configuration accepted: %s", bad)
		}
	}
	if err := os.WriteFile(path, []byte(`{"names":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = parseRuntimeFlags(t, "--skills-config", path)
	if err != nil || cfg.SkillSelection.Names == nil {
		t.Fatalf("explicit empty skill selection lost: %+v %v", cfg, err)
	}
}
