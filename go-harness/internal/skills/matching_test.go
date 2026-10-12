package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestSkillLearningMatchingAccessUsesRuntimeVisibilityAndTransaction(t *testing.T) {
	ctx := context.Background()
	ws, other, global := testutil.CanonicalTempDir(t), testutil.CanonicalTempDir(t), t.TempDir()
	local, foreign := filepath.Join(ws, "skills"), filepath.Join(other, "skills")
	for _, root := range []string{local, foreign, global} {
		writeSkill(t, root, "same-name", "Compare full JSON content")
	}
	writeSkill(t, global, "global-only", "A global procedure")
	writeSkill(t, local, "local-only", "A local procedure")
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{
		{ID: "global", Root: global, Scope: harness.SkillScopeGlobal},
		{ID: "project", Root: local, Workspace: ws, Scope: harness.SkillScopeWorkspace},
		{ID: "foreign", Root: foreign, Workspace: other, Scope: harness.SkillScopeWorkspace},
	}}
	r, db := registry(t, cfg)
	for _, pair := range [][2]string{{"global", "same-name"}, {"global", "global-only"}, {"project", "same-name"}, {"project", "local-only"}, {"foreign", "same-name"}} {
		installEnabled(t, r, pair[0], pair[1])
	}
	for _, mode := range []string{"workspace", "global", "none", "selected", "unknown-name"} {
		selection := harness.SkillSelection{Workspace: ws, IncludeGlobal: mode == "global"}
		if mode == "none" {
			selection.Names = []string{}
		}
		if mode == "selected" {
			selection.Names = []string{"local-only"}
		}
		if mode == "unknown-name" {
			selection.Names = []string{"absent"}
		}
		access, err := r.NewMatchAccess(selection)
		if err != nil {
			t.Fatal(err)
		}
		// Mutating the original host policy cannot enlarge this startup copy.
		if len(selection.Names) != 0 {
			selection.Names[0] = "same-name"
		}
		tx, err := db.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		inv, err := access.InventoryTx(ctx, tx, ws)
		if mode == "unknown-name" {
			_ = tx.Rollback()
			if !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("unknown runtime selection became authority", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if mode == "global" {
			want = 3
		} else if mode == "none" {
			want = 0
		} else if mode == "selected" {
			want = 1
		}
		if len(inv.Skills) != want {
			t.Fatalf("%s=%+v", mode, inv)
		}
		for _, skill := range inv.Skills {
			if skill.Target.SourceID == "foreign" || skill.Target.Name == "same-name" && skill.Target.SourceID != "project" || !skill.Head.Exists || !skill.Head.Enabled || skill.Head.StateVersion < 1 {
				t.Fatal("wrong scoped identity or head", skill)
			}
			if _, err = access.BodyTx(ctx, tx, inv, skill); err != nil {
				t.Fatal(err)
			}
		}
		_ = tx.Rollback()
	}
	if err := r.SetEnabled(ctx, "project", "same-name", false); err != nil {
		t.Fatal(err)
	}
	access, err := r.NewMatchAccess(harness.SkillSelection{IncludeGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := access.InventoryTx(ctx, tx, ws)
	_ = tx.Rollback()
	if err != nil || slices.ContainsFunc(inv.Skills, func(s MatchSkill) bool { return s.Target.Name == "same-name" }) {
		t.Fatal("disabled workspace did not shadow global", inv, err)
	}
}

func TestSkillLearningMatchingNewCapabilityTargetUsesHostScopeAndProtectsNames(t *testing.T) {
	for _, mode := range []string{"workspace", "global", "global-denied", "foreign", "builtin", "none", "selected", "collision", "disabled", "tombstone", "global-collision"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			ws, other, global := testutil.CanonicalTempDir(t), testutil.CanonicalTempDir(t), t.TempDir()
			local, foreign := filepath.Join(ws, "skills"), filepath.Join(other, "skills")
			for _, root := range []string{local, foreign, global} {
				writeSkill(t, root, "structured-json-audit", "An existing reusable procedure")
			}
			cfg := harness.SkillsConfig{Sources: []harness.SkillSource{
				{ID: "project", Root: local, Scope: harness.SkillScopeWorkspace, Workspace: ws},
				{ID: "foreign", Root: foreign, Scope: harness.SkillScopeWorkspace, Workspace: other},
				{ID: "global", Root: global, Scope: harness.SkillScopeGlobal},
			}}
			if mode == "builtin" {
				cfg.BuiltinSources = []string{"project"}
			}
			r, db := registry(t, cfg)
			selection := harness.SkillSelection{IncludeGlobal: mode == "global" || mode == "global-collision"}
			if mode == "none" {
				selection.Names = []string{}
			} else if mode == "selected" {
				selection.Names = []string{"other-name"}
			}
			if mode == "collision" || mode == "disabled" || mode == "tombstone" || mode == "global-collision" {
				source := "project"
				if mode == "global-collision" {
					source = "global"
				}
				installEnabled(t, r, source, "structured-json-audit")
				if mode == "disabled" {
					if err := r.SetEnabled(ctx, source, "structured-json-audit", false); err != nil {
						t.Fatal(err)
					}
				} else if mode == "tombstone" {
					if err := r.WithPublication(ctx, source, "structured-json-audit", func(pub *PublicationTx) error {
						head, err := pub.Current()
						if err == nil {
							_, err = pub.Publish("", head, false)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			access, err := r.NewMatchAccess(selection)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.DB().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			source := "project"
			if mode == "global" || mode == "global-denied" {
				source = "global"
			} else if mode == "foreign" {
				source = "foreign"
			}
			target, err := access.NewCapabilityTargetTx(ctx, tx, ws, source, "structured-json-audit")
			want := mode == "workspace" || mode == "global"
			if (err == nil) != want || want && (target.SourceID != source || target.Name != "structured-json-audit") {
				t.Fatal(mode, target, err)
			}
			if _, err = access.NewCapabilityTargetTx(ctx, tx, ws, source, "../other-name"); err == nil {
				t.Fatal("name suggestion granted a path")
			}
		})
	}
}

func TestSkillLearningMatchingAccessRejectsDuplicateOversizedAndCorruptBodies(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"duplicate", "oversized", "corrupt", "oversized-metadata", "immutable"} {
		t.Run(mode, func(t *testing.T) {
			ws := testutil.CanonicalTempDir(t)
			root, second := filepath.Join(ws, "skills"), filepath.Join(ws, "second")
			body := "Read complete structured text"
			if mode == "oversized" {
				body = strings.Repeat("x", MaxMatchBodyBytes)
			}
			writeSkill(t, root, "sample", body)
			cfg := harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "one", Root: root, Workspace: ws, Scope: harness.SkillScopeWorkspace}}}
			if mode == "duplicate" {
				writeSkill(t, second, "sample", body)
				cfg.Sources = append(cfg.Sources, harness.SkillSource{ID: "two", Root: second, Workspace: ws, Scope: harness.SkillScopeWorkspace})
			}
			r, db := registry(t, cfg)
			for _, source := range cfg.Sources {
				installEnabled(t, r, source.ID, "sample")
			}
			if mode == "corrupt" {
				if _, err := db.DB().Exec(`UPDATE df_skill_files SET data='different' WHERE path='SKILL.md'`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "oversized-metadata" {
				if _, err := db.DB().Exec(`UPDATE df_skill_versions SET metadata=? WHERE name='sample'`, strings.Repeat("x", maxMatchMetadataBytes+1)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "immutable" {
				if err := os.WriteFile(filepath.Join(root, "sample", "SKILL.md"), []byte(skillText("sample", "PRIVATE_MUTABLE_REPLACEMENT")), 0600); err != nil {
					t.Fatal(err)
				}
			}
			access, err := r.NewMatchAccess(harness.SkillSelection{})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.DB().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			inv, err := access.InventoryTx(ctx, tx, ws)
			if mode == "duplicate" || mode == "oversized-metadata" {
				if err == nil {
					t.Fatal("invalid metadata or name collision resolved by guess")
				}
				return
			}
			if err != nil || len(inv.Skills) != 1 {
				t.Fatal(inv, err)
			}
			got, err := access.BodyTx(ctx, tx, inv, inv.Skills[0])
			if (err == nil) != (mode == "immutable") || mode == "immutable" && strings.Contains(got, "PRIVATE_MUTABLE") {
				t.Fatal("invalid or mutable skill body exposed", got, err)
			}
		})
	}
}

func TestSkillLearningMatchingBodyGrantCannotBeForgedOrReusedAcrossTransactions(t *testing.T) {
	ctx := context.Background()
	ws := testutil.CanonicalTempDir(t)
	root := filepath.Join(ws, "skills")
	writeSkill(t, root, "sample", "Authorized instruction")
	r, db := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: ws}}})
	installEnabled(t, r, "project", "sample")
	access, err := r.NewMatchAccess(harness.SkillSelection{})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := access.InventoryTx(ctx, tx, ws)
	if err != nil || len(inv.Skills) != 1 {
		t.Fatal(inv, err)
	}
	skill := inv.Skills[0]
	changed := skill
	changed.Target.SourceIdentity = strings.Repeat("f", 64)
	inv.Skills[0] = changed
	for _, grant := range []MatchInventory{inv, {Skills: []MatchSkill{skill}}} {
		if _, err = access.BodyTx(ctx, tx, grant, changed); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatal("mutated inventory descriptor was authorized", err)
		}
	}
	if _, err = access.BodyTx(ctx, tx, MatchInventory{Skills: []MatchSkill{skill}}, skill); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("public inventory fields forged body access", err)
	}
	if _, err = access.BodyTx(ctx, tx, inv, skill); err != nil {
		t.Fatal("fixed grant changed with public slice mutation", err)
	}
	_ = tx.Rollback()
	tx, err = db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = access.BodyTx(ctx, tx, inv, skill); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("previous transaction grant leaked", err)
	}
}

func TestSkillLearningMatchingAccessPinsBuiltinOverlayAndABAHead(t *testing.T) {
	ctx := context.Background()
	ws := testutil.CanonicalTempDir(t)
	root := filepath.Join(ws, "skills")
	writeSkill(t, root, "sample", "Original builtin instruction")
	r, db := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Workspace: ws, Scope: harness.SkillScopeWorkspace}}, BuiltinSources: []string{"project"}})
	installEnabled(t, r, "project", "sample")
	access, err := r.NewMatchAccess(harness.SkillSelection{})
	if err != nil {
		t.Fatal(err)
	}
	read := func() MatchSkill {
		t.Helper()
		tx, err := db.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		inv, err := access.InventoryTx(ctx, tx, ws)
		if err != nil || len(inv.Skills) != 1 {
			t.Fatal(inv, err)
		}
		return inv.Skills[0]
	}
	before := read()
	if err = r.SetEnabled(ctx, "project", "sample", false); err != nil {
		t.Fatal(err)
	}
	if err = r.SetEnabled(ctx, "project", "sample", true); err != nil {
		t.Fatal(err)
	}
	after := read()
	if before.Head.Version != after.Head.Version || before.Head.StateVersion == after.Head.StateVersion || !after.Builtin {
		t.Fatal("ABA head or builtin policy lost", before, after)
	}
	if _, err = db.DB().Exec(`UPDATE df_skill_control SET managed=1 WHERE name='sample';
 INSERT INTO df_skill_overlays SELECT source_key,name,current_version,'fixture' FROM df_skill_installs WHERE name='sample'`); err != nil {
		t.Fatal(err)
	}
	overlay := read()
	if overlay.OverlayBase == nil || overlay.OverlayBase.Version != before.Head.Version || !overlay.Head.Managed {
		t.Fatal("overlay lineage omitted", overlay)
	}
}
