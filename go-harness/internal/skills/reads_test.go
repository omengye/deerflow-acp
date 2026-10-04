package skills

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestReadObservationsRequireValidatedContentAndInheritSubset(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "research", "private body")
	writeFile(t, filepath.Join(root, "research", "guide.txt"), []byte("private reference"))
	writeFile(t, filepath.Join(root, "research", "binary.dat"), []byte{0, 255})
	r, db := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "workspace", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: root}}})
	installed := installEnabled(t, r, "workspace", "research")
	original := snapshot(t, r, harness.SkillSelection{Workspace: root})
	var reads []ReadObservation
	observed := original.WithReadObserver(func(_ context.Context, read ReadObservation) error {
		reads = append(reads, read)
		return nil
	})
	if _, err := observed.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := observed.Files(ctx, "research"); err != nil {
		t.Fatal(err)
	}
	if _, err := observed.ReadFile(ctx, "research", "SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Get(ctx, "research"); err != nil || len(reads) != 0 {
		t.Fatalf("metadata, raw/operator reads were observed: %v %v", reads, err)
	}
	child, err := observed.Subset([]string{"research"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = child.Get(ctx, "research"); err != nil {
		t.Fatal(err)
	}
	if _, err = child.ReadFileTool().InvokableRun(ctx, `{"skill":"research","path":"guide.txt"}`); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 || reads[0].Kind != "skill_load" || reads[1].Kind != "support_read" || reads[0].File.Path != "SKILL.md" || reads[1].File.Path != "guide.txt" {
		t.Fatalf("observed reads=%+v", reads)
	}
	for _, read := range reads {
		if read.Ref != installed.Ref || read.Scope != harness.SkillScopeWorkspace || read.File.Hash == "" {
			t.Fatalf("unpinned observation: %+v", read)
		}
	}
	for _, args := range []string{`{"skill":"research","path":"binary.dat"}`, `{"skill":"research","path":"missing"}`, `{"skill":"research","path":"../guide.txt"}`} {
		if _, err = child.ReadFileTool().InvokableRun(ctx, args); err == nil {
			t.Fatal("invalid read succeeded")
		}
	}
	if _, err = db.DB().Exec(`UPDATE df_skill_files SET data=? WHERE path='SKILL.md'`, []byte("corrupted")); err != nil {
		t.Fatal(err)
	}
	if _, err = child.Get(ctx, "research"); err == nil || len(reads) != 2 {
		t.Fatalf("failed read emitted evidence: %d %v", len(reads), err)
	}
}

func TestReadObserverFailureDoesNotReturnUnrecordedContent(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "research", "private body")
	r, _ := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "workspace", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: root}}})
	installEnabled(t, r, "workspace", "research")
	failure := errors.New("fixture evidence store failed")
	s := snapshot(t, r, harness.SkillSelection{Workspace: root}).WithReadObserver(func(context.Context, ReadObservation) error { return failure })
	if loaded, err := s.Get(context.Background(), "research"); !errors.Is(err, failure) || loaded.Content != "" {
		t.Fatalf("content escaped unrecorded: %+v %v", loaded, err)
	}
	if content, err := s.ReadFileTool().InvokableRun(context.Background(), `{"skill":"research","path":"SKILL.md"}`); !errors.Is(err, failure) || content != "" {
		t.Fatalf("file escaped unrecorded: %q %v", content, err)
	}
}
