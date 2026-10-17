package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillDiscoveryAcceptsBOMAndReportsMalformedPackage(t *testing.T) {
	root := t.TempDir()
	for name, text := range map[string]string{
		"encoded": "\xef\xbb\xbf---\r\nname: encoded\r\ndescription: Test encoding\r\n---\r\nInstructions\r\n",
		"broken":  "no frontmatter",
	} {
		dir := filepath.Join(root, "skills", "custom", name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := InspectConfiguredSkills(filepath.Join(root, "config.yaml"), map[string]any{"skills": map[string]any{"path": "skills", "enabled": true}})
	if err != nil || len(cfg.Install) != 1 || cfg.Install[0].Directory != "encoded" {
		t.Fatalf("discovery: %+v %v", cfg, err)
	}
	if len(cfg.Diagnostics) != 1 || !strings.Contains(cfg.Diagnostics[0].Path, "broken") || cfg.Diagnostics[0].Message == "" {
		t.Fatalf("missing actionable diagnostic: %+v", cfg.Diagnostics)
	}
}
