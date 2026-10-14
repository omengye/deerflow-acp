package skills

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestDisabledSameScopeDuplicateDoesNotBlockSelection(t *testing.T) {
	for _, scope := range []harness.SkillScope{harness.SkillScopeGlobal, harness.SkillScopeWorkspace} {
		for _, active := range []string{"a", "b", "both", "neither"} {
			t.Run(string(scope)+"/"+active, func(t *testing.T) {
				ctx := context.Background()
				ws := t.TempDir()
				cfg := harness.SkillsConfig{}
				for _, id := range []string{"a", "b"} {
					root := t.TempDir()
					if scope == harness.SkillScopeWorkspace {
						root = filepath.Join(ws, id)
					}
					writeSkill(t, root, "shared", id)
					source := harness.SkillSource{ID: id, Root: root, Scope: scope}
					if scope == harness.SkillScopeWorkspace {
						source.Workspace = ws
					}
					cfg.Sources = append(cfg.Sources, source)
				}
				r, db := registry(t, cfg)
				for _, id := range []string{"a", "b"} {
					installEnabled(t, r, id, "shared")
					if err := r.SetEnabled(ctx, id, "shared", active == id || active == "both"); err != nil {
						t.Fatal(err)
					}
				}
				selection := harness.SkillSelection{Workspace: ws, IncludeGlobal: true}
				snap, snapErr := r.Snapshot(ctx, selection)
				access, err := r.NewMatchAccess(selection)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := db.DB().BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				inv, invErr := access.InventoryTx(ctx, tx, ws)
				_ = tx.Rollback()
				if active == "both" {
					for _, err := range []error{snapErr, invErr} {
						var conflict *harness.SkillNameConflictError
						if !errors.As(err, &conflict) || conflict.Name != "shared" {
							t.Fatalf("expected typed conflict: %v", err)
						}
					}
					return
				}
				if snapErr != nil || invErr != nil {
					t.Fatal(snapErr, invErr)
				}
				want := 1
				if active == "neither" {
					want = 0
				}
				refs := snap.Refs()
				if len(refs) != want || len(inv.Skills) != want {
					t.Fatal(refs, inv)
				}
				if want == 1 && (refs[0].SourceID != active || inv.Skills[0].Target.SourceID != active) {
					t.Fatal("wrong active source", refs, inv)
				}
			})
		}
	}
}
