package desktopconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundledSkillDriftPreservesLocalChanges(t *testing.T) {
	opts := setup(t)
	source := filepath.Join(opts.Resources, "skills", "public", "skill-creator")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	old := "---\nname: skill-creator\ndescription: Author skills\n---\nOld instructions\n"
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, opts, "init", nil)
	installed := filepath.Join(opts.UserData, "skills", "public", "skill-creator", "SKILL.md")
	local := old + "Local customization\n"
	if err := os.WriteFile(installed, []byte(local), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(old+"New distribution guidance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	doc := run(t, opts, "snapshot", nil)
	if !strings.Contains(strings.Join(doc["skill_source_diagnostics"].([]string), "\n"), "Bundled skill skill-creator differs") {
		t.Fatal("missing upgrade diagnostic", doc["skill_source_diagnostics"])
	}
	got, err := os.ReadFile(installed)
	if err != nil || string(got) != local {
		t.Fatal("local customization overwritten", err)
	}
}
