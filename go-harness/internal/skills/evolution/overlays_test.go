package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func reopenBuiltin(t *testing.T, f *fixture) {
	t.Helper()
	validator := f.service.validator
	if err := f.registry.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.registry, err = skills.NewRegistry(context.Background(), f.cfg, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	f.service, err = New(context.Background(), f.registry, validator)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinOverlayReviewedLifecycleAndPinnedIdentity(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	selection := harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace}
	legacy, err := f.registry.Snapshot(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	original := legacy.Refs()[0]
	f.cfg.BuiltinSources = []string{"project"}
	reopenBuiltin(t, f)
	current, err := f.registry.Snapshot(ctx, selection)
	if err != nil || current.Refs()[0] != original {
		t.Fatalf("builtin policy changed source identity: %+v %v", current, err)
	}
	p := f.submit(t, f.request(t, "overlay", "Reviewed built-in instructions"))
	if p.OverlayBase == nil || *p.OverlayBase != original {
		t.Fatalf("missing proposal provenance: %+v", p)
	}
	var overlays int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_overlays`).Scan(&overlays); err != nil || overlays != 0 {
		t.Fatalf("unapproved overlay activated: %d %v", overlays, err)
	}
	p, err = f.service.Approve(ctx, p.ID, review(p))
	if err != nil || p.PublishedRevision != 2 {
		t.Fatalf("approve: %+v %v", p, err)
	}
	assertLive := func(version string) {
		t.Helper()
		s, err := f.registry.Snapshot(ctx, selection)
		if err != nil || len(s.Refs()) != 1 || s.Refs()[0].Version != version {
			t.Fatalf("overlay snapshot: %+v %v", s, err)
		}
		records, err := f.registry.List(ctx, selection)
		if err != nil || len(records) != 1 || !records[0].Builtin || !records[0].Managed || records[0].OverlayBase == nil || *records[0].OverlayBase != original {
			t.Fatalf("overlay projection: %+v %v", records, err)
		}
	}
	assertLive(p.CandidateVersion)
	sourceFile := filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md")
	data, err := os.ReadFile(sourceFile)
	if err != nil || string(data) != string(skillFiles("Original instructions")["SKILL.md"]) {
		t.Fatalf("publication wrote distribution source: %q %v", data, err)
	}
	// Distribution updates and restart do not overwrite the managed overlay.
	if err = os.WriteFile(sourceFile, skillFiles("New upstream release")["SKILL.md"], 0600); err != nil {
		t.Fatal(err)
	}
	reopenBuiltin(t, f)
	if err = f.registry.InstallConfigured(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	assertLive(p.CandidateVersion)
	old, err := f.registry.Restore(ctx, selection, legacy.Refs())
	if err != nil {
		t.Fatal(err)
	}
	data, err = old.ReadFile(ctx, "research", "SKILL.md")
	if err != nil || string(data) != string(skillFiles("Original instructions")["SKILL.md"]) {
		t.Fatalf("old run lost immutable built-in: %q %v", data, err)
	}
	rollback, err := f.service.Rollback(ctx, Rollback{SourceID: "project", Name: "research", RequestKey: "restore-builtin", Revision: 1, Expected: f.head(t), Actor: "operator"})
	if err != nil || rollback.OverlayBase == nil || *rollback.OverlayBase != original {
		t.Fatalf("rollback lost provenance: %+v %v", rollback, err)
	}
	assertLive(original.Version)
	deletion := f.request(t, "delete-overlay", "")
	deletion.Action, deletion.Files = "delete", nil
	d := f.submit(t, deletion)
	if _, err = f.service.Approve(ctx, d.ID, review(d)); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(sourceFile); err != nil {
		t.Fatal(err)
	}
	// Provenance stays durable even if a host no longer labels this source as
	// builtin; removing that label cannot rewrite the historical overlay base.
	f.cfg.BuiltinSources = nil
	reopenBuiltin(t, f)
	if err = f.registry.InstallConfigured(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	if f.head(t).Exists {
		t.Fatal("deleted overlay reappeared")
	}
	if _, err = f.service.Rollback(ctx, Rollback{SourceID: "project", Name: "research", RequestKey: "restore-overlay", Revision: 2, Expected: f.head(t), Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	assertLive(p.CandidateVersion)
	history, err := f.service.History(ctx, "project", "research", 0, 20)
	if err != nil || len(history) != 5 {
		t.Fatalf("history: %+v %v", history, err)
	}
	for _, entry := range history[1:] {
		if entry.OverlayBase == nil || *entry.OverlayBase != original {
			t.Fatalf("history lost original: %+v", entry)
		}
	}
}

func TestBuiltinOverlayAtomicityAndIdentityValidation(t *testing.T) {
	for _, failure := range []string{"overlay-insert", "proposal-source", "proposal-version", "base-content"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t, nil)
			f.cfg.BuiltinSources = []string{"project"}
			reopenBuiltin(t, f)
			ctx := context.Background()
			before := f.head(t)
			p := f.submit(t, f.request(t, "candidate", "Replacement"))
			switch failure {
			case "overlay-insert":
				_, err := f.db.DB().Exec(`CREATE TRIGGER overlay_failure BEFORE INSERT ON df_skill_overlays BEGIN SELECT RAISE(ABORT,'overlay commit failure'); END`)
				if err != nil {
					t.Fatal(err)
				}
			case "proposal-source", "proposal-version":
				if failure == "proposal-source" {
					p.OverlayBase.SourceID = "another-source"
				} else {
					p.OverlayBase.Version, p.OverlayBase.Hash = p.CandidateVersion, p.CandidateVersion
				}
				body, _ := json.Marshal(p)
				if _, err := f.db.DB().Exec(`UPDATE df_skill_proposals SET payload=? WHERE id=?`, body, p.ID); err != nil {
					t.Fatal(err)
				}
			case "base-content":
				if _, err := f.db.DB().Exec(`UPDATE df_skill_files SET data=? WHERE version=?`, []byte("corrupt"), before.Version); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.Approve(ctx, p.ID, review(p)); err == nil {
				t.Fatal("invalid overlay published")
			}
			if f.head(t) != before {
				t.Fatal("failed transaction changed head")
			}
			for _, table := range []string{"df_skill_overlays", "df_skill_publications"} {
				var n int
				if err := f.db.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
					t.Fatalf("partial %s: %d %v", table, n, err)
				}
			}
			if failure == "overlay-insert" {
				if _, err := f.db.DB().Exec(`DROP TRIGGER overlay_failure`); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.Approve(ctx, p.ID, review(p)); err != nil {
					t.Fatal("atomic failure was not retryable", err)
				}
			}
		})
	}
}

func TestBuiltinOverlayRequiresImportedOrigin(t *testing.T) {
	f := newFixture(t, nil)
	f.cfg.BuiltinSources = []string{"project"}
	reopenBuiltin(t, f)
	request := Request{SourceID: "project", Name: "new-skill", RequestKey: "not-an-import", Action: "create", Reason: "Cannot invent a built-in", Files: map[string][]byte{"SKILL.md": []byte("---\nname: new-skill\ndescription: New skill\n---\nInstructions\n")}}
	if _, err := f.service.Submit(context.Background(), request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("invented builtin origin: %v", err)
	}
	var proposals int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_proposals`).Scan(&proposals); err != nil || proposals != 0 {
		t.Fatalf("failed create retained a proposal: %d %v", proposals, err)
	}
}

func TestBuiltinOverlayPolicyChangeRequiresNewReview(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	before := f.head(t)
	p := f.submit(t, f.request(t, "before-builtin", "Candidate without builtin provenance"))
	if p.OverlayBase != nil {
		t.Fatal("ordinary candidate had overlay provenance")
	}
	f.cfg.BuiltinSources = []string{"project"}
	reopenBuiltin(t, f)
	stale, err := f.service.Approve(ctx, p.ID, review(p))
	if !errors.Is(err, ErrConflict) || stale.Status != "stale" || stale.OverlayBase != nil || f.head(t) != before {
		t.Fatalf("changed policy silently rewrote review: %+v %v", stale, err)
	}
	resubmitted := f.submit(t, f.request(t, "after-builtin", "New candidate with explicit builtin origin"))
	if resubmitted.OverlayBase == nil || resubmitted.OverlayBase.Version != before.Version {
		t.Fatalf("new review missing origin: %+v", resubmitted)
	}
	if _, err = f.service.Approve(ctx, resubmitted.ID, review(resubmitted)); err != nil {
		t.Fatal(err)
	}
}
