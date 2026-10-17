package skills

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestBOMFrontmatterPreservesInstalledBytes(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(strings.ReplaceAll(newline, "\n", "LF"), func(t *testing.T) {
			root := t.TempDir()
			data := []byte("\xef\xbb\xbf" + strings.ReplaceAll(skillText("encoded", "Read instructions."), "\n", newline))
			writeFile(t, filepath.Join(root, "encoded", "SKILL.md"), data)
			r, _ := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "local", Root: root, Scope: harness.SkillScopeGlobal}}})
			installEnabled(t, r, "local", "encoded")
			s := snapshot(t, r, harness.SkillSelection{IncludeGlobal: true})
			got, err := s.ReadFile(context.Background(), "encoded", "SKILL.md")
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("original bytes changed: %q %v", got, err)
			}
			if _, err = s.Get(context.Background(), "encoded"); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, prefix := range []string{"\xef\xbb\xbf\xef\xbb\xbf", " \xef\xbb\xbf", "\xff\xfe"} {
		if _, _, err := ParseSkillDescription([]byte(prefix + skillText("encoded", "Instructions"))); err == nil {
			t.Fatalf("accepted invalid prefix %q", prefix)
		}
	}
}
