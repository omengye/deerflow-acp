package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestFileAuthorityExternalChangesAndNoFallback(t *testing.T) {
	r, db, selection := publicationFixture(t)
	ctx := context.Background()
	old := snapshot(t, r, selection)
	head := publicationHead(t, r)
	if _, err := r.SetEnabledChecked(ctx, harness.SkillEnablement{Target: sourceTarget(r.sources["project"], "research"), Expected: head, Enabled: true, RequestKey: "managed-flag", Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	root := r.sources["project"].config.Root
	writeSkill(t, root, "research", "External file change")
	fresh := snapshot(t, r, selection)
	if fresh.Refs()[0].Version == old.Refs()[0].Version {
		t.Fatal("external edit ignored")
	}
	text, err := fresh.ReadFile(ctx, "research", "SKILL.md")
	if err != nil || !strings.Contains(string(text), "External file change") {
		t.Fatal(string(text), err)
	}
	if _, err := db.DB().Exec(`UPDATE df_skill_installs SET current_version=? WHERE name='research'`, old.Refs()[0].Version); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, r, selection).Refs()[0].Version; got != fresh.Refs()[0].Version {
		t.Fatal("database pointer overrode directory")
	}
	for _, mode := range []string{"invalid", "missing"} {
		if mode == "invalid" {
			writeFile(t, filepath.Join(root, "research", "SKILL.md"), []byte("broken header"))
		} else if err := os.Remove(filepath.Join(root, "research", "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		s := snapshot(t, r, selection)
		if len(s.Refs()) != 0 || s.SourceWarnings() == "" {
			t.Fatal("silent fallback", mode)
		}
		if _, err := r.Restore(ctx, selection, old.Refs()); err != nil {
			t.Fatal("lost explicit historical resume", err)
		}
	}
	writeSkill(t, root, "research", "Repaired on disk")
	if len(snapshot(t, r, selection).Refs()) != 1 {
		t.Fatal("repair not observed")
	}
	var stored int
	if err := db.DB().QueryRow(`SELECT COALESCE(sum(length(data)),0) FROM df_skill_files`).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("new file bodies stored in database", stored, err)
	}
}

func TestFileAuthorityMigrationAndBuiltin(t *testing.T) {
	r, _, selection := publicationFixture(t)
	ctx := context.Background()
	old := snapshot(t, r, selection)
	hidden := candidate(t, r, "Old hidden database overlay")
	err := r.WithPublication(ctx, "project", "research", func(p *PublicationTx) error {
		v, err := p.Stage(hidden)
		if err != nil {
			return err
		}
		h, err := p.Current()
		if err != nil {
			return err
		}
		_, err = p.moveHead(h, v, true, true, "", "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh := snapshot(t, r, selection)
	if fresh.Refs()[0].Version != old.Refs()[0].Version {
		t.Fatal("migration preferred hidden content")
	}
	data, err := os.ReadFile(filepath.Join(r.sources["project"].config.Root, "research", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "Original instructions") {
		t.Fatal("migration overwrote source")
	}
	r.sources["project"].builtin = true
	err = r.WithPublication(ctx, "project", "research", func(p *PublicationTx) error {
		h, e := p.Current()
		if e != nil {
			return e
		}
		_, e = p.Publish(hidden.Version(), h, true)
		return e
	})
	if !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("builtin hidden overlay allowed", err)
	}
	if _, err := r.PrepareEdit(ctx, fresh.Refs()[0], "owner", "builtin", fanrenEdits()[:1]); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("builtin direct edit allowed", err)
	}
}

func TestFileAuthorityPublicationWritesAndRecovers(t *testing.T) {
	r, _, selection := publicationFixture(t)
	ctx := context.Background()
	old := snapshot(t, r, selection)
	base := publicationHead(t, r)
	next := candidate(t, r, "Published to files")
	after := publishCandidate(t, r, next, base)
	data, err := os.ReadFile(filepath.Join(r.sources["project"].config.Root, "research", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "Published to files") {
		t.Fatal("publication only changed database", err)
	}
	if snapshot(t, r, selection).Refs()[0].Version != after.Version {
		t.Fatal("publication not indexed")
	}
	err = r.WithPublication(ctx, "project", "research", func(p *PublicationTx) error { _, e := p.Publish(base.Version, after, true); return e })
	if err != nil {
		t.Fatal(err)
	}
	if snapshot(t, r, selection).Refs()[0].Version != old.Refs()[0].Version {
		t.Fatal("file rollback not loaded")
	}
	// Commit only an authorized journal to simulate a crash before filesystem I/O.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := r.catalogTx(ctx, tx, r.sources["project"], "research")
	head, err := p.Current()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Publish(next.Version(), head, true); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot(t, r, selection).Refs()) != 0 {
		t.Fatal("pending publication used stale cache")
	}
	if err = r.RecoverPublications(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot(t, r, selection).Refs()[0].Version != next.Version() {
		t.Fatal("approved publication not recovered")
	}
}

func TestFileAuthorityLegacyDatabaseOnlyUnavailable(t *testing.T) {
	r, db, selection := publicationFixture(t)
	if _, err := db.DB().Exec(`UPDATE df_skill_control SET import_directory='',managed=1 WHERE name='research'`); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, r, selection)
	if len(s.Refs()) != 0 || !strings.Contains(s.SourceWarnings(), "legacy_database_only_requires_migration") {
		t.Fatal("unbound hidden version loaded")
	}
}

func TestFileAuthorityDeletedSkillsDoNotWarnOrLoadInCurrentSnapshot(t *testing.T) {
	for _, mode := range []string{"directory", "legacy_database_only"} {
		t.Run(mode, func(t *testing.T) {
			r, db, selection := publicationFixture(t)
			ctx := context.Background()
			old := snapshot(t, r, selection)
			if err := r.Delete(ctx, "project", "research"); err != nil {
				t.Fatal(err)
			}
			if mode == "legacy_database_only" {
				if _, err := db.DB().Exec(`UPDATE df_skill_control SET import_directory='' WHERE name='research'`); err != nil {
					t.Fatal(err)
				}
			}
			current := snapshot(t, r, selection)
			if len(current.Refs()) != 0 || current.SourceWarnings() != "" {
				t.Fatalf("deleted skill is still advertised: refs=%+v warnings=%q", current.Refs(), current.SourceWarnings())
			}
			history, err := r.QueryCatalog(ctx, harness.SkillQuery{Status: "deleted", Limit: 10})
			if err != nil || len(history.Skills) != 1 || history.Skills[0].Head.Exists {
				t.Fatalf("deletion history was lost: %+v %v", history, err)
			}
			restored, err := r.Restore(ctx, selection, old.Refs())
			if err != nil || len(restored.Refs()) != 1 {
				t.Fatalf("explicit pinned resume was lost: %v", err)
			}
		})
	}
}

func TestFileAuthorityUnboundSourceStillWarns(t *testing.T) {
	r, db, selection := publicationFixture(t)
	if _, err := db.DB().Exec(`INSERT INTO df_skill_source_status VALUES(?,?,'','unavailable','','legacy_database_only_requires_migration')`, r.sources["project"].identity, "unbound"); err != nil {
		t.Fatal(err)
	}
	current := snapshot(t, r, selection)
	if len(current.Refs()) != 1 || !strings.Contains(current.SourceWarnings(), "Skill unbound ") {
		t.Fatal("missing install/control row suppressed a legitimate source warning")
	}
	selection.Names = []string{"research"}
	if got := snapshot(t, r, selection).SourceWarnings(); got != "" {
		t.Fatalf("unselected source leaked a warning: %q", got)
	}
}

func TestFileAuthorityNoReplaceAndNoSameNameFallback(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{"file", "dir"} {
		from, to := filepath.Join(root, kind+"-from"), filepath.Join(root, kind+"-to")
		if kind == "dir" {
			if err := os.Mkdir(from, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(to, 0700); err != nil {
				t.Fatal(err)
			}
		} else {
			writeFile(t, from, []byte("first"))
			writeFile(t, to, []byte("second"))
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		err = renameNoReplace(r, kind+"-from", kind+"-to")
		_ = r.Close()
		if err == nil {
			t.Fatal("exclusive rename replaced destination", kind)
		}
		if _, err = os.Stat(from); err != nil {
			t.Fatal("source lost on conflict", err)
		}
	}
	a, b := t.TempDir(), t.TempDir()
	writeSkill(t, a, "research", "Authoritative A")
	writeSkill(t, b, "research", "Other directory B")
	r, _ := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "a", Root: a, Scope: harness.SkillScopeGlobal}, {ID: "b", Root: b, Scope: harness.SkillScopeGlobal}}})
	installEnabled(t, r, "a", "research")
	installEnabled(t, r, "b", "research")
	if err := os.Remove(filepath.Join(a, "research", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, r, harness.SkillSelection{IncludeGlobal: true}); len(got.Refs()) != 0 {
		t.Fatal("missing source fell through to other same-name copy")
	}
	if err := r.SetEnabled(context.Background(), "a", "research", false); err != nil {
		t.Fatal(err)
	}
	got := snapshot(t, r, harness.SkillSelection{IncludeGlobal: true})
	if len(got.Refs()) != 1 || got.Refs()[0].SourceID != "b" {
		t.Fatal("explicitly disabled old source blocked authorized replacement")
	}
}

func TestFileAuthorityPendingExportDoesNotBreakCatalog(t *testing.T) {
	r, _, _, selection := managedExportFixture(t)
	ctx := context.Background()
	base := publishCandidate(t, r, candidate(t, r, "Base file publication"), SkillHead{})
	next := candidate(t, r, "Next file publication")
	if err := r.WithPublication(ctx, "project", "research", func(p *PublicationTx) error { _, e := p.Stage(next); return e }); err != nil {
		t.Fatal(err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := r.catalogTx(ctx, tx, r.sources["project"], "research")
	if _, err = p.Publish(next.Version(), base, true); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = r.ReconcileExports(ctx, false); err != nil {
		t.Fatal(err)
	}
	detail, err := r.GetCatalogSkill(ctx, sourceTarget(r.sources["project"], "research"))
	if err != nil || detail.Skill.Head != base || detail.Skill.SourceStatus != "unavailable" || detail.Skill.Export != nil {
		t.Fatal("pending export broke catalog", detail, err)
	}
	if len(snapshot(t, r, selection).Refs()) != 0 {
		t.Fatal("pending export changed authority")
	}
	if err = r.RecoverPublications(ctx); err != nil {
		t.Fatal(err)
	}
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
}
