package evolution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestFileAuthorityApprovalWaitsForVerifiedCommit(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	selection := harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace}
	old, err := f.registry.Snapshot(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	p := f.submit(t, f.request(t, "crash-after-files", "New authoritative bytes"))
	if _, err = f.db.DB().Exec(`CREATE TRIGGER commit_failure BEFORE UPDATE ON df_skill_installs BEGIN SELECT RAISE(ABORT,'simulated index commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.Approve(ctx, p.ID, review(p))
	if err == nil || got.Status != "publishing" || !got.FilePublicationPending {
		t.Fatal("uncommitted publication reported success", got, err)
	}
	file := filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md")
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), "New authoritative bytes") {
		t.Fatal("fault not after actual file commit", err)
	}
	fresh, err := f.registry.Snapshot(ctx, selection)
	if err != nil || len(fresh.Refs()) != 0 {
		t.Fatal("pending files loaded", err)
	}
	if _, err = f.registry.Restore(ctx, selection, old.Refs()); err != nil {
		t.Fatal(err)
	}
	page, err := f.service.QueryProposals(ctx, harness.SkillQuery{Status: "publishing", Limit: 10})
	if err != nil || len(page.Proposals) != 1 {
		t.Fatal("publishing projection", page, err)
	}
	history, err := f.service.History(ctx, "project", "research", 0, 10)
	if err != nil || history[1].FileStatus != "pending" {
		t.Fatal(history, err)
	}
	if _, err = f.db.DB().Exec(`DROP TRIGGER commit_failure`); err != nil {
		t.Fatal(err)
	}
	reopenBuiltin(t, f) // Reopen registry/service, without regenerating or approving.
	if err = f.registry.RecoverPublications(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = f.service.Get(ctx, p.ID)
	if err != nil || got.Status != "published" || got.FilePublicationPending {
		t.Fatal("recovered publication incomplete", got, err)
	}
	fresh, err = f.registry.Snapshot(ctx, selection)
	if err != nil || fresh.Refs()[0].Version != p.CandidateVersion {
		t.Fatal("recovered wrong version", err)
	}
}

func TestFileAuthorityPublicationAbortPreservesExternalChanges(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	base := f.head(t)
	p := f.submit(t, f.request(t, "blocked", "Approved candidate"))
	blocker := filepath.Join(f.cfg.Sources[0].Root, ".deerflow-publication-stage")
	if err := os.WriteFile(blocker, []byte("block staging"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.Approve(ctx, p.ID, review(p))
	if !errors.Is(err, harness.ErrReconciliationRequired) || got.Status != "publishing" {
		t.Fatal(got, err)
	}
	jobs, err := f.registry.HostFilePublications(ctx, p.Target)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	file := filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md")
	if err = os.WriteFile(file, skillFiles("External later edit")["SKILL.md"], 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.registry.ReconcilePublication(ctx, p.Target, jobs[0].ID, base, "abort", "operator"); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("abort overwrote external edit", err)
	}
	if err = os.WriteFile(file, skillFiles("Original instructions")["SKILL.md"], 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.registry.ReconcilePublication(ctx, p.Target, jobs[0].ID, base, "abort", "operator"); err != nil {
		t.Fatal(err)
	}
	got, err = f.service.Get(ctx, p.ID)
	if err != nil || got.Status != "failed" || got.FilePublicationPending {
		t.Fatal("abort status", got, err)
	}
	history, err := f.service.History(ctx, "project", "research", 0, 10)
	if err != nil || history[1].FileStatus != "aborted" {
		t.Fatal(history, err)
	}
	if err = os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	next := f.submit(t, f.request(t, "after-abort", "Later approved files"))
	if _, err = f.service.Approve(ctx, next.ID, review(next)); err != nil {
		t.Fatal(err)
	}
	history, err = f.service.History(ctx, "project", "research", 0, 10)
	if err != nil || history[1].FileStatus != "aborted" || history[2].FileStatus != "applied" {
		t.Fatal("reused aborted completion identity", history, err)
	}
}
