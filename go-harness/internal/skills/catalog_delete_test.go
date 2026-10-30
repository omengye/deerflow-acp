package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestCatalogDirectDeletePersistsWithoutReviewOrSourceRemoval(t *testing.T) {
	for _, builtin := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom", true: "builtin"}[builtin], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			writeSkill(t, root, "research", "original")
			cfg := harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeGlobal}}}
			if builtin {
				cfg.BuiltinSources = []string{"project"}
			}
			r, db := registry(t, cfg)
			if err := r.InstallConfigured(ctx, "project", "research", true); err != nil {
				t.Fatal(err)
			}
			entry := catalogItem(t, r, "project", "research")
			selection := harness.SkillSelection{IncludeGlobal: true}
			old := snapshot(t, r, selection)
			req := harness.SkillDeletion{Target: entry.Target, Expected: entry.Head, RequestKey: "remove", Actor: "desktop"}
			result, err := r.DeleteChecked(ctx, req)
			want := harness.SkillHead{StateVersion: entry.Head.StateVersion + 1, Managed: true}
			if err != nil || result.Replayed || result.AppliedHead != want || result.Skill.Head != want || result.Skill.Record != nil {
				t.Fatalf("delete: %+v %v", result, err)
			}
			current, err := r.QueryCatalog(ctx, harness.SkillQuery{Status: "current", Limit: 10})
			if err != nil || len(current.Skills) != 0 || len(snapshot(t, r, selection).Refs()) != 0 || len(old.Refs()) != 1 {
				t.Fatalf("deleted installation remains current or changed pinned task: %+v %v", current, err)
			}
			if _, err = os.Stat(filepath.Join(root, "research", "SKILL.md")); err != nil {
				t.Fatal("catalog removal changed source files", err)
			}
			if err = r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewRegistry(ctx, cfg, db.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			writeSkill(t, root, "research", "source changed after deletion")
			if err = reopened.InstallConfigured(ctx, "project", "research", true); err != nil {
				t.Fatal(err)
			}
			if err = reopened.RefreshSources(ctx, selection); err != nil {
				t.Fatal(err)
			}
			replay, err := reopened.DeleteChecked(ctx, req)
			if err != nil || !replay.Replayed || replay.AppliedHead != want || replay.Skill.Head != want || len(snapshot(t, reopened, selection).Refs()) != 0 {
				t.Fatalf("restart/import resurrected removal or lost request: %+v %v", replay, err)
			}
			restored, err := reopened.Restore(ctx, selection, old.Refs())
			if err != nil || len(restored.Refs()) != 1 {
				t.Fatalf("old task cannot restore its immutable version: %v", err)
			}
		})
	}
}

func TestCatalogDirectDeleteAuditRollbackAndConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	r, db, _, _ := managedExportFixture(t)
	publishCandidate(t, r, candidate(t, r, "original"), SkillHead{})
	entry := catalogItem(t, r, "project", "research")
	req := harness.SkillDeletion{Target: entry.Target, Expected: entry.Head, RequestKey: "delete", Actor: "desktop"}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_delete BEFORE INSERT ON df_skill_management_requests BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DeleteChecked(ctx, req); err == nil {
		t.Fatal("audit failure was ignored")
	}
	after := catalogItem(t, r, "project", "research")
	if after.Head != entry.Head || after.Export.StateToken != entry.Export.StateToken {
		t.Fatal("failed deletion changed head or export queue")
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_delete`); err != nil {
		t.Fatal(err)
	}
	bad := req
	bad.Target.SourceIdentity = strings.Repeat("f", 64)
	if _, err := r.DeleteChecked(ctx, bad); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("foreign target accepted: %v", err)
	}
	bad = req
	bad.Expected.Enabled = !bad.Expected.Enabled
	if _, err := r.DeleteChecked(ctx, bad); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("changed expected head accepted: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"first", "second"} {
		wg.Go(func() {
			request := req
			request.RequestKey = key
			_, err := r.DeleteChecked(ctx, request)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, harness.ErrSkillConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("delete writers: %d wins, %d conflicts", wins, conflicts)
	}
}
