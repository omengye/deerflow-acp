package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

func TestSkillTriggerNativeSuiteLoadingUsesExplicitHostFileAndSourceGrant(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "skill-behavior-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	path := filepath.Join(root, "suite.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBehaviorSuite(config, "suite.json", []string{"managed-skills"})
	if err != nil || len(loaded.Suites) != 1 || loaded.Suites[0].Version != skillbehavior.TriggerVersion || len(loaded.Suites[0].Cases) != 4 {
		t.Fatal(loaded, err)
	}
	digest, _ := skillbehavior.Digest(loaded.Suites[0])
	if _, err := LoadBehaviorSuite(config, "suite.json", []string{"another-source"}); err == nil {
		t.Fatal("native host suite bypassed generation source grant")
	}
	invalid := []byte(strings.Replace(string(data), `"kind": "trigger_negative"`, `"kind": "trigger_positive"`, 1))
	if err := os.WriteFile(path, invalid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBehaviorSuite(config, "suite.json", []string{"managed-skills"}); err == nil {
		t.Fatal("changed file with missing negative oracle accepted")
	}
	if saved, err := skillbehavior.Digest(loaded.Suites[0]); err != nil || saved != digest {
		t.Fatal("host suite changed after startup snapshot", saved, err)
	}
}
