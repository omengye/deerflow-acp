package skills

import (
	"context"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSnapshotSubsetPinsVersionsAndCannotExpandVisibility(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "research", "ORIGINAL_RESEARCH")
	writeSkill(t, root, "private", "PRIVATE_SKILL")
	r, _ := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "workspace", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: root}}})
	installEnabled(t, r, "workspace", "research")
	installEnabled(t, r, "workspace", "private")
	parent, err := r.Snapshot(ctx, harness.SkillSelection{Workspace: root, Names: []string{"research"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.Subset([]string{"research"})
	if err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "research", "CHANGED_RESEARCH")
	installEnabled(t, r, "workspace", "research")
	if child.Refs()[0].Version != parent.Refs()[0].Version {
		t.Fatal("child acquired a newer version")
	}
	loaded, err := child.Get(ctx, "research")
	if err != nil || !strings.Contains(loaded.Content, "ORIGINAL_RESEARCH") {
		t.Fatalf("child content=%+v err=%v", loaded, err)
	}
	if _, err := child.Subset([]string{"private"}); err == nil {
		t.Fatal("child acquired hidden parent skill")
	}
	empty, err := parent.Subset([]string{})
	if err != nil || len(empty.Refs()) != 0 {
		t.Fatal("empty selection granted skills")
	}
	if _, err := empty.Get(ctx, "research"); err == nil {
		t.Fatal("empty child read parent skill")
	}
	all, err := parent.Subset(nil)
	if err != nil || len(all.Refs()) != 1 {
		t.Fatal("nil selection did not inherit pinned skills")
	}
}
