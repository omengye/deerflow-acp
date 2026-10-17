package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestAdoptionJournalNeverRecreatesMissingSource(t *testing.T) {
	r, _, selection := publicationFixture(t)
	ctx := context.Background()
	root := r.sources["project"].config.Root
	files := map[string][]byte{"SKILL.md": []byte(skillText("adopted", "Approved content"))}
	writeSkill(t, root, "adopted", "Approved content")
	c, err := r.PrepareCandidate("adopted", files)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := r.catalogTx(ctx, tx, r.sources["project"], "adopted")
	v, err := p.Stage(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Publish(v, SkillHead{}, true); err != nil {
		t.Fatal(err)
	}
	if !p.publication.Adopt {
		t.Fatal("missing adoption mode")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "adopted")
	if err = os.Rename(path, filepath.Join(root, "operator-backup")); err != nil {
		t.Fatal(err)
	}
	if err = r.finishPublication(ctx, r.sources["project"], *p.publication); !errors.Is(err, harness.ErrReconciliationRequired) {
		t.Fatal("missing adoption source accepted", err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("adoption recreated removed user files", err)
	}
	s, err := r.Snapshot(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range s.Refs() {
		if ref.Name == "adopted" {
			t.Fatal("uncommitted adoption loaded")
		}
	}
}
