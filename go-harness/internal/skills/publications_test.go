package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	store "github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func publicationFixture(t *testing.T) (*Registry, *store.Store, harness.SkillSelection) {
	t.Helper()
	ws := t.TempDir()
	root := filepath.Join(ws, "skills")
	writeSkill(t, root, "research", "Original instructions")
	r, db := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: ws}}})
	if err := r.InstallConfigured(context.Background(), "project", "research", true); err != nil {
		t.Fatal(err)
	}
	return r, db, harness.SkillSelection{Workspace: ws}
}

func publicationHead(t *testing.T, r *Registry) SkillHead {
	t.Helper()
	var head SkillHead
	err := r.WithPublication(context.Background(), "project", "research", func(tx *PublicationTx) error {
		var err error
		head, err = tx.Current()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func candidate(t *testing.T, r *Registry, body string) *Candidate {
	t.Helper()
	c, err := r.PrepareCandidate("research", map[string][]byte{"SKILL.md": []byte(skillText("research", body))})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func publishCandidate(t *testing.T, r *Registry, c *Candidate, before SkillHead) SkillHead {
	t.Helper()
	var head SkillHead
	err := r.WithPublication(context.Background(), "project", "research", func(tx *PublicationTx) error {
		version, err := tx.Stage(c)
		if err == nil {
			head, err = tx.Publish(version, before, true)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestPublicationPointerSnapshotRestartAndTombstone(t *testing.T) {
	r, db, selection := publicationFixture(t)
	ctx := context.Background()
	original := snapshot(t, r, selection)
	head := publicationHead(t, r)
	updated := candidate(t, r, "Reviewed replacement")
	current := publishCandidate(t, r, updated, head)
	if !current.Managed || current.StateVersion != head.StateVersion+1 {
		t.Fatalf("missing publication state: %+v", current)
	}
	if got := snapshot(t, r, selection).Refs()[0].Version; got != updated.Version() {
		t.Fatalf("new run did not see publication: %s", got)
	}
	old, err := original.ReadFile(ctx, "research", "SKILL.md")
	if err != nil || string(old) != skillText("research", "Original instructions") {
		t.Fatalf("active snapshot changed: %q %v", old, err)
	}
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{r.sources["project"].config}}
	// No old package file is required after publishing; the source root remains
	// configured to authorize access to the same persisted identity.
	if err = os.Remove(filepath.Join(cfg.Sources[0].Root, "research", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.InstallConfigured(ctx, "project", "research", false); err != nil {
		t.Fatal(err)
	}
	if got := publicationHead(t, reopened); got != current {
		t.Fatalf("startup overwrote publication or enablement: %+v", got)
	}
	// Rollback restores the original content with a new state version.
	err = reopened.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
		var err error
		current, err = tx.Publish(head.Version, current, true)
		return err
	})
	if err != nil || current.Version != head.Version || current.StateVersion <= head.StateVersion {
		t.Fatalf("rollback: %+v %v", current, err)
	}
	err = reopened.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
		_, err := tx.Publish(updated.Version(), head, true)
		return err
	})
	if !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("ABA content revision bypassed CAS: %v", err)
	}
	err = reopened.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
		var err error
		current, err = tx.Publish("", current, false)
		return err
	})
	if err != nil || current.Exists || !current.Managed || current.Enabled {
		t.Fatalf("delete tombstone: %+v %v", current, err)
	}
	writeSkill(t, cfg.Sources[0].Root, "research", "Stale filesystem copy")
	if err = reopened.InstallConfigured(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	if got := publicationHead(t, reopened); got != current || len(snapshot(t, reopened, selection).Refs()) != 0 {
		t.Fatalf("startup resurrected deleted skill: %+v", got)
	}
	if _, err = reopened.Install(ctx, "project", "research"); !errors.Is(err, ErrManagedSkill) {
		t.Fatalf("ordinary import silently replaced managed skill: %v", err)
	}
	if _, err = reopened.Restore(ctx, selection, original.Refs()); err != nil {
		t.Fatalf("delete lost pinned historical version: %v", err)
	}
}

func TestPublicationTransactionRollsBackContentPointerAndAudit(t *testing.T) {
	r, db, _ := publicationFixture(t)
	ctx := context.Background()
	before := publicationHead(t, r)
	c := candidate(t, r, "Uncommitted candidate")
	if _, err := db.DB().Exec(`CREATE TABLE publication_audit(version TEXT); CREATE TRIGGER fail_audit BEFORE INSERT ON publication_audit BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`); err != nil {
		t.Fatal(err)
	}
	err := r.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
		version, err := tx.Stage(c)
		if err != nil {
			return err
		}
		if _, err = tx.Publish(version, before, true); err != nil {
			return err
		}
		_, err = tx.SQL().ExecContext(ctx, `INSERT INTO publication_audit VALUES(?)`, version)
		return err
	})
	if err == nil || publicationHead(t, r) != before {
		t.Fatalf("publication escaped failed audit transaction: %v", err)
	}
	for _, table := range []string{"df_skill_versions", "df_skill_files", "publication_audit"} {
		var count int
		if err = db.DB().QueryRow(`SELECT count(*) FROM `+table+` WHERE version=?`, c.Version()).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial candidate in %s: %d %v", table, count, err)
		}
	}
}

func TestPublicationConcurrentReviewCASAndCorruptRollback(t *testing.T) {
	r, db, _ := publicationFixture(t)
	ctx := context.Background()
	base := publicationHead(t, r)
	a, b := candidate(t, r, "Candidate A"), candidate(t, r, "Candidate B")
	// Two registries have distinct mutexes and share only their SQLite store.
	other, err := NewRegistry(ctx, harness.SkillsConfig{Sources: []harness.SkillSource{r.sources["project"].config}}, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errorsOut := make(chan error, 2)
	for i, c := range []*Candidate{a, b} {
		registry := []*Registry{r, other}[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			errorsOut <- registry.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
				version, err := tx.Stage(c)
				if err == nil {
					_, err = tx.Publish(version, base, true)
				}
				return err
			})
		}()
	}
	wg.Wait()
	close(errorsOut)
	wins, conflicts := 0, 0
	for err := range errorsOut {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrPublicationConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("reviews overwrote each other: wins=%d conflicts=%d", wins, conflicts)
	}
	current := publicationHead(t, r)
	if _, err = db.DB().Exec(`UPDATE df_skill_files SET data='corrupt' WHERE version=?`, base.Version); err != nil {
		t.Fatal(err)
	}
	err = r.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
		_, err := tx.Publish(base.Version, current, true)
		return err
	})
	if err == nil || publicationHead(t, r) != current {
		t.Fatalf("corrupted rollback target was published: %v", err)
	}
}

func TestCandidateCopiesAndRejectsAmbiguousPaths(t *testing.T) {
	r, _, _ := publicationFixture(t)
	files := map[string][]byte{"SKILL.md": []byte(skillText("research", "Valid instructions"))}
	c, err := r.PrepareCandidate("research", files)
	if err != nil {
		t.Fatal(err)
	}
	version := c.Version()
	files["SKILL.md"][0] = 'x'
	copy := c.Files()
	copy["SKILL.md"][0] = 'y'
	if c.Version() != version || c.Files()["SKILL.md"][0] != '-' {
		t.Fatal("caller mutated immutable candidate")
	}
	for _, bad := range []string{"../outside", "a/../b", "C:/outside", "NUL.txt", "refs/.git/config", "refs\\guide"} {
		files := c.Files()
		files[bad] = []byte("value")
		if _, err := r.PrepareCandidate("research", files); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted candidate path %q: %v", bad, err)
		}
	}
	for _, paths := range [][]string{{"refs", "refs/file.md"}, {"refs/A.md", "refs/a.md"}, {"Refs/a.md", "refs/b.md"}} {
		files := c.Files()
		for _, name := range paths {
			files[name] = []byte("value")
		}
		if _, err := r.PrepareCandidate("research", files); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted ambiguous tree %v: %v", paths, err)
		}
	}
}
