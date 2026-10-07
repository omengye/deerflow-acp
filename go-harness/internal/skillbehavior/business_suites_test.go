package skillbehavior

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundledBusinessSuites(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{"code-documentation", "newsletter-generation"} {
		data, err := os.ReadFile(filepath.Join(root, "resources", "skill-behavior", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		suite, err := Parse(data)
		if err != nil || suite.Name != name || suite.SourceID != "python-public" || len(suite.Cases) != 2 || suite.Cases[0].Kind != "targeted" || suite.Cases[1].Kind != "regression" {
			t.Fatalf("suite=%+v error=%v", suite, err)
		}
		installed, err := os.ReadFile(filepath.Join(root, "skills", "public", name, "SKILL.md"))
		if err != nil || !strings.Contains(string(installed), "name: "+name) {
			t.Fatalf("missing skill %s: %v", name, err)
		}
	}
}
