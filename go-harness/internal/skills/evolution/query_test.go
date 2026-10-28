package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestEvolutionQueriesRejectCatalogNameContains(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	query := harness.SkillQuery{NameContains: "rese", Limit: 1}
	for name, call := range map[string]func() error{
		"proposals":   func() error { _, err := f.service.QueryProposals(ctx, query); return err },
		"history":     func() error { _, err := f.service.QueryHistory(ctx, query); return err },
		"signals":     func() error { _, err := f.service.QuerySignals(ctx, query); return err },
		"generation":  func() error { _, err := f.service.QueryGeneration(ctx, query); return err },
		"auto-patch":  func() error { _, err := f.service.QueryAutoPatch(ctx, query); return err },
		"discoveries": func() error { _, err := f.service.QueryDiscoveries(ctx, query); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("catalog-only substring filter was not rejected: %v", err)
			}
		})
	}
}

func TestProposalPagesBindFiltersAndConfiguredSourceIdentity(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	first := f.submit(t, f.request(t, "one", "Candidate one"))
	second := f.submit(t, f.request(t, "two", "Candidate two"))
	third := f.submit(t, f.request(t, "three", "Candidate three"))
	want := map[string]bool{first.ID: true, second.ID: true, third.ID: true}
	query := harness.SkillQuery{Limit: 1, Status: "pending_review"}
	status, err := f.service.AutoPatchStatus(ctx)
	if err != nil || status.Readiness == nil || status.Readiness.NeedsAttention != 3 || status.Readiness.WaitingCandidates != 0 {
		t.Fatal("attention count must cover all pages", status, err)
	}
	onlyWaiting := false
	waiting, err := f.service.QueryProposals(ctx, harness.SkillQuery{Limit: 1, Status: "pending_review", NeedsAttention: &onlyWaiting})
	if err != nil || len(waiting.Proposals) != 0 {
		t.Fatal(waiting, err)
	}
	var cursor string
	for range 4 {
		page, err := f.service.QueryProposals(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page.Proposals {
			if p.Disposition == nil || !p.Disposition.NeedsAttention || p.Disposition.Reason != "manual_review" {
				t.Fatal(p)
			}
			if !want[p.ID] || p.Diff != "" {
				t.Fatalf("unexpected list record %+v", p)
			}
			delete(want, p.ID)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
		query.Cursor = cursor
	}
	if len(want) != 0 || cursor == "" {
		t.Fatalf("missing records: %v", want)
	}
	query.NeedsAttention = &onlyWaiting
	if _, err := f.service.QueryProposals(ctx, query); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("changed attention filter accepted cursor", err)
	}
	query.NeedsAttention = nil
	query.Status = "rejected"
	if _, err := f.service.QueryProposals(ctx, query); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("changed filters accepted cursor: %v", err)
	}
	if _, err := f.service.Approve(ctx, first.ID, review(first)); err != nil {
		t.Fatal(err)
	}
	page, err := f.service.QueryHistory(ctx, harness.SkillQuery{Limit: 1})
	if err != nil || len(page.Publications) != 1 || page.Next == "" || page.Publications[0].Action != "baseline" {
		t.Fatalf("history first: %+v %v", page, err)
	}
	next, err := f.service.QueryHistory(ctx, harness.SkillQuery{Limit: 1, Cursor: page.Next})
	if err != nil || len(next.Publications) != 1 || next.Next != "" || next.Publications[0].ProposalID != first.ID {
		t.Fatalf("history second: %+v %v", next, err)
	}
	if _, err = f.service.QueryProposals(ctx, harness.SkillQuery{Limit: 1, Cursor: page.Next}); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("history cursor read proposal page: %v", err)
	}
	if err = f.registry.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f.cfg.Sources[0].Root = root
	f.cfg.Sources[0].Workspace = root
	f.registry, err = skills.NewRegistry(ctx, f.cfg, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	f.service, err = New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.service.QueryProposals(ctx, harness.SkillQuery{Limit: 100})
	if err != nil || len(list.Proposals) != 0 {
		t.Fatalf("old source leaked candidates: %+v %v", list, err)
	}
	status, err = f.service.AutoPatchStatus(ctx)
	if err != nil || status.Readiness.NeedsAttention != 0 {
		t.Fatal("revoked source leaked attention", status, err)
	}
	history, err := f.service.QueryHistory(ctx, harness.SkillQuery{Limit: 100})
	if err != nil || len(history.Publications) != 0 {
		t.Fatalf("old source leaked publications: %+v %v", history, err)
	}
	if _, err = f.service.QueryProposals(ctx, harness.SkillQuery{Limit: 1, Status: "pending_review", Cursor: cursor}); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("old source cursor accepted: %v", err)
	}
}

func TestProposalCorruptionCannotChangeReviewedCandidate(t *testing.T) {
	for _, kind := range []string{"candidate", "base", "diff", "behavior", "changed-files"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, nil)
			ctx := context.Background()
			p := f.submit(t, f.request(t, "edit", "Reviewed candidate"))
			base := f.head(t)
			changed := p
			switch kind {
			case "candidate":
				changed.CandidateVersion = base.Version
			case "base":
				changed.Base.Enabled = !changed.Base.Enabled
			case "diff":
				changed.Diff = "Unrelated display text"
			case "behavior":
				changed.Evaluation.Behavior = "failed"
			case "changed-files":
				changed.ChangedFiles = []string{"some-other-file.md"}
			}
			data, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.db.DB().Exec(`UPDATE df_skill_proposals SET payload=? WHERE id=?`, data, p.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.Approve(ctx, p.ID, review(p)); err == nil {
				t.Fatal("corrupt proposal published")
			}
			if f.head(t) != base {
				t.Fatal("corrupt proposal changed installation")
			}
		})
	}
}
