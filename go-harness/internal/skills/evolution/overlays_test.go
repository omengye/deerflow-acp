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

func TestBuiltinPublicationRequiresExplicitCustomCopy(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	selection := harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace}
	old, err := f.registry.Snapshot(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.BuiltinSources = []string{"project"}
	reopenBuiltin(t, f)
	p := f.submit(t, f.request(t, "builtin-candidate", "Candidate for explicit custom copy"))
	if _, err = f.service.Approve(ctx, p.ID, review(p)); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("hidden overlay approved", err)
	}
	sourceFile := filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md")
	if err = os.WriteFile(sourceFile, skillFiles("New distribution files")["SKILL.md"], 0600); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.registry.Snapshot(ctx, selection)
	if err != nil || fresh.Refs()[0].Version == old.Refs()[0].Version {
		t.Fatal("distribution files ignored", err)
	}
	historical, err := f.registry.Restore(ctx, selection, old.Refs())
	if err != nil {
		t.Fatal(err)
	}
	data, err := historical.ReadFile(ctx, "research", "SKILL.md")
	if err != nil || string(data) != string(skillFiles("Original instructions")["SKILL.md"]) {
		t.Fatal("explicit historical restore changed", err)
	}
	var overlays int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_overlays`).Scan(&overlays); err != nil || overlays != 0 {
		t.Fatal("hidden overlay created", err)
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
				if _, err := f.service.Approve(ctx, p.ID, review(p)); !errors.Is(err, harness.ErrPermissionDenied) {
					t.Fatal("builtin must remain read-only after retry", err)
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
	if _, err = f.service.Approve(ctx, resubmitted.ID, review(resubmitted)); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("builtin still requires explicit custom copy", err)
	}
}
