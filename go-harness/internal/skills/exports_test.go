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

func TestManagedExportRejectsLinkedDestination(t *testing.T) {
	r, _, cfg, selection := managedExportFixture(t)
	outside := t.TempDir()
	createDirectoryLink(t, outside, filepath.Join(cfg.ExportRoot, ".deerflow-exports"))
	head := publishCandidate(t, r, candidate(t, r, "Approved package"), SkillHead{})
	if err := r.SyncExports(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := exportState(t, r, selection, "pending")
	if state.ErrorCode != "filesystem_unavailable" || state.Head != head {
		t.Fatalf("linked export destination accepted: %+v", state)
	}
	files, err := os.ReadDir(outside)
	if err != nil || len(files) != 0 {
		t.Fatalf("export escaped configured root: %+v %v", files, err)
	}
}

func TestManagedSourcePolicyRejectsInvalidAuthority(t *testing.T) {
	r, db, cfg, _ := managedExportFixture(t)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unconfigured", "duplicate", "builtin", "no-export-root"} {
		t.Run(mode, func(t *testing.T) {
			modified := cfg
			switch mode {
			case "unconfigured":
				modified.ManagedSources = []string{"elsewhere"}
			case "duplicate":
				modified.ManagedSources = []string{"project", "project"}
			case "builtin":
				modified.BuiltinSources = []string{"project"}
			case "no-export-root":
				modified.ExportRoot = ""
			}
			bad, err := NewRegistry(context.Background(), modified, db.DB())
			if err == nil {
				_ = bad.Close()
				t.Fatal("invalid managed source authority accepted")
			}
		})
	}
}

func managedExportFixture(t *testing.T) (*Registry, *store.Store, harness.SkillsConfig, harness.SkillSelection) {
	t.Helper()
	workspace := t.TempDir()
	root := filepath.Join(workspace, "managed")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: workspace}}, ManagedSources: []string{"project"}, ExportRoot: root}
	r, db := registry(t, cfg)
	return r, db, cfg, harness.SkillSelection{Workspace: workspace}
}

func exportState(t *testing.T, r *Registry, selection harness.SkillSelection, status string) harness.SkillExportState {
	t.Helper()
	items, err := r.ListExports(context.Background(), selection)
	if err != nil || len(items) != 1 || items[0].Status != status {
		t.Fatalf("export state: %+v %v", items, err)
	}
	return items[0]
}

func TestManagedSourceExportRebuildAndTombstone(t *testing.T) {
	r, db, cfg, selection := managedExportFixture(t)
	ctx := context.Background()
	if current := snapshot(t, r, selection); len(current.Refs()) != 0 {
		t.Fatal("unreviewed disk package became active")
	}
	c := candidate(t, r, "Approved generated skill")
	head := publishCandidate(t, r, c, SkillHead{})
	queued := exportState(t, r, selection, "pending")
	if queued.Head != head || queued.Directory != "" {
		t.Fatalf("publication did not queue exact head: %+v", queued)
	}
	if err := r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	synced := exportState(t, r, selection, "synced")
	viewFile := filepath.Join(cfg.ExportRoot, filepath.FromSlash(synced.Directory), "files", "SKILL.md")
	if data, err := os.ReadFile(viewFile); err != nil || string(data) != skillText("research", "Approved generated skill") {
		t.Fatalf("exported bytes: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(cfg.Sources[0].Root, "research", "SKILL.md")); err != nil || string(data) != skillText("research", "Approved generated skill") {
		t.Fatalf("publication did not write authoritative path: %q %v", data, err)
	}
	// A restart checks complete views and retains their paths when unchanged.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	r, err = NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.ReconcileExports(ctx, false); err != nil {
		t.Fatal(err)
	}
	exportState(t, r, selection, "pending")
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	if got := exportState(t, r, selection, "synced"); got.Directory != synced.Directory || got.Head != head {
		t.Fatalf("idempotent export: %+v", got)
	}
	// Disk edits are preserved as old views; a new verified view is generated.
	if err = os.WriteFile(viewFile, []byte("Local edit is not a skill publication"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = r.ReconcileExports(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	repaired := exportState(t, r, selection, "synced")
	if repaired.Directory == synced.Directory || publicationHead(t, r) != head {
		t.Fatalf("disk modification changed authority: %+v", repaired)
	}
	if data, err := os.ReadFile(viewFile); err != nil || string(data) != "Local edit is not a skill publication" {
		t.Fatalf("disk edit was overwritten: %q %v", data, err)
	}
	if err = r.Delete(ctx, "project", "research"); err != nil {
		t.Fatal(err)
	}
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	deleted := exportState(t, r, selection, "synced")
	if deleted.Head.Exists || deleted.Head.Enabled || deleted.Head.StateVersion != head.StateVersion+1 {
		t.Fatalf("tombstone export: %+v", deleted)
	}
	if _, err = os.Stat(filepath.Join(cfg.ExportRoot, filepath.FromSlash(deleted.Directory), "files")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tombstone exported active content: %v", err)
	}
	if hidden, err := r.ListExports(ctx, harness.SkillSelection{Workspace: t.TempDir()}); err != nil || len(hidden) != 0 {
		t.Fatalf("cross-workspace exports: %+v %v", hidden, err)
	}
}

func TestManagedExportFailureDoesNotUndoPublicationAndRetryIsDurable(t *testing.T) {
	r, db, cfg, selection := managedExportFixture(t)
	ctx := context.Background()
	// A regular file blocks the reserved export directory without requiring
	// platform-specific ACLs or changing anything outside the temporary fixture.
	blocker := filepath.Join(cfg.ExportRoot, ".deerflow-exports")
	if err := os.WriteFile(blocker, []byte("reserved by fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	head := publishCandidate(t, r, candidate(t, r, "Published despite unavailable export"), SkillHead{})
	for attempt := 1; attempt <= 3; attempt++ {
		if err := r.SyncExports(ctx); err != nil {
			t.Fatal(err)
		}
		status := "pending"
		if attempt == 3 {
			status = "failed"
		}
		state := exportState(t, r, selection, status)
		if state.Attempts != attempt || state.Head != head || state.ErrorCode != "filesystem_unavailable" || publicationHead(t, r) != head {
			t.Fatalf("failed export changed publication: %+v", state)
		}
		if attempt != 3 {
			if state.RetryAfter == nil {
				t.Fatal("retry is unbounded")
			}
			if _, err := db.DB().Exec(`UPDATE df_skill_exports SET retry_after_ns=0`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	r, err = NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.ReconcileExports(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	exportState(t, r, selection, "failed")
	if err = os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err = r.ReconcileExports(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	exportState(t, r, selection, "synced")
	if publicationHead(t, r) != head {
		t.Fatal("repair replayed publication")
	}
}

func TestManagedExportQueueSharesPublicationTransaction(t *testing.T) {
	r, db, selection := publicationFixture(t)
	ctx := context.Background()
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{r.sources["project"].config}, ExportRoot: t.TempDir()}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	r, err = NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	before := publicationHead(t, r)
	c := candidate(t, r, "Built-in overlay")
	if _, err = db.DB().Exec(`CREATE TRIGGER export_failure BEFORE INSERT ON df_skill_exports BEGIN SELECT RAISE(ABORT,'queue unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	err = r.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
		version, err := tx.Stage(c)
		if err == nil {
			_, err = tx.Publish(version, before, true)
		}
		return err
	})
	if err == nil || publicationHead(t, r) != before {
		t.Fatalf("outbox failure partially published: %v", err)
	}
	for _, table := range []string{"df_skill_exports", "df_skill_overlays"} {
		var n int
		if err = db.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial %s: %d %v", table, n, err)
		}
	}
	if _, err = db.DB().Exec(`DROP TRIGGER export_failure`); err != nil {
		t.Fatal(err)
	}
	publishCandidate(t, r, c, before)
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	exportState(t, r, selection, "synced")
}

func TestManagedExportConcurrentHeadChangeAndRootRebind(t *testing.T) {
	r, db, cfg, selection := managedExportFixture(t)
	ctx := context.Background()
	first := publishCandidate(t, r, candidate(t, r, "First publication"), SkillHead{})
	second := candidate(t, r, "Second publication")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Go(func() { results <- r.SyncExports(ctx) })
	wg.Go(func() {
		results <- r.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
			version, err := tx.Stage(second)
			if err == nil {
				_, err = tx.Publish(version, first, true)
			}
			return err
		})
	})
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	latest := exportState(t, r, selection, "synced")
	if latest.Head.Version != second.Version() {
		t.Fatalf("old export superseded current head: %+v", latest)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	oldRoot := cfg.ExportRoot
	cfg.ExportRoot = t.TempDir()
	other, err := NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if items, err := other.ListExports(ctx, selection); err != nil || len(items) != 0 {
		t.Fatalf("new root inherited stale view: %+v %v", items, err)
	}
	if err = other.ReconcileExports(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err = other.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	newView := exportState(t, other, selection, "synced")
	for _, location := range []string{filepath.Join(oldRoot, filepath.FromSlash(latest.Directory)), filepath.Join(cfg.ExportRoot, filepath.FromSlash(newView.Directory))} {
		if _, err := os.Stat(filepath.Join(location, "files", "SKILL.md")); err != nil {
			t.Fatal(err)
		}
	}
	if publicationHead(t, other) != latest.Head {
		t.Fatal("export root rebind changed source identity or publication")
	}
}
