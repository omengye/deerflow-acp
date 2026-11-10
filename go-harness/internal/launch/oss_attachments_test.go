package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestAppOssSkillIsDiscoveredEnabledAndHasRunnableEntrypoint(t *testing.T) {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := InspectConfiguredSkills(filepath.Join(repo, "config.yaml"), map[string]any{
		"skills": map[string]any{"enabled": true, "path": "./skills"},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, install := range cfg.Install {
		if install.SourceID == "python-custom" && install.Directory == "oss-attachments" {
			found = install.Enabled
		}
	}
	if !found {
		t.Fatal("App OSS skill missing or disabled in startup discovery")
	}
	files := make(map[string][]byte)
	for _, name := range []string{"SKILL.md", "skill-runtime.json", "scripts/fetch_oss.py"} {
		files[name], err = os.ReadFile(filepath.Join(repo, "skills", "custom", "oss-attachments", name))
		if err != nil {
			t.Fatal(err)
		}
	}
	manifest, present, err := skills.ParseRuntimeManifest(files)
	if err != nil || !present || len(manifest.Python.Entrypoints) != 1 || manifest.Python.Entrypoints[0] != "scripts/fetch_oss.py" {
		t.Fatalf("App OSS skill runtime declaration invalid: %v", err)
	}
}
