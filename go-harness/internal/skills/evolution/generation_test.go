package evolution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type fixtureGenerator struct {
	id    string
	calls atomic.Int32
	body  func(context.Context, skillmaintenance.GenerationInput) (skillmaintenance.Candidate, error)
}

func (g *fixtureGenerator) Identity() string { return g.id }
func (g *fixtureGenerator) Generate(ctx context.Context, in skillmaintenance.GenerationInput) (skillmaintenance.Candidate, error) {
	if err := generationCharge(ctx, "generation"); err != nil {
		return skillmaintenance.Candidate{}, err
	}
	g.calls.Add(1)
	if g.body != nil {
		return g.body(ctx, in)
	}
	return skillmaintenance.Candidate{Reason: "Clarify verification steps", Files: skillFiles("Verify sources before summarizing")}, nil
}

type fixtureGenerationValidator struct {
	calls    atomic.Int32
	failures atomic.Int32
	before   func(context.Context)
	id       string
}

func (v *fixtureGenerationValidator) Identity() string { return v.id }
func (v *fixtureGenerationValidator) Validate(ctx context.Context, in ValidationInput) (Report, error) {
	v.calls.Add(1)
	if v.before != nil {
		v.before(ctx)
	}
	for _, stage := range []string{"security", "quality"} {
		if err := generationCharge(ctx, stage); err != nil {
			return Report{}, err
		}
	}
	if v.failures.Load() > 0 {
		v.failures.Add(-1)
		return Report{}, harness.ErrSkillValidation
	}
	report := validReport(in)
	report.ValidatorVersion = v.id
	return report, nil
}

func generationCharge(ctx context.Context, stage string) error {
	b, ok := skillmaintenance.BrokerFromContext(ctx)
	if !ok {
		return harness.ErrPermissionDenied
	}
	r, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: stage, InputTokens: 100, MaxOutputTokens: 100, InputDigest: strings.Repeat("a", 64), ModelIdentity: strings.Repeat("b", 64)})
	if err != nil {
		return err
	}
	return b.Settle(ctx, r, skillmaintenance.Usage{InputTokens: 10, OutputTokens: 5, Known: true})
}

func generationFixture(t *testing.T) (*fixture, *fixtureGenerator, *fixtureGenerationValidator) {
	t.Helper()
	v := &fixtureGenerationValidator{id: "review-fixture-v1"}
	f := newFixture(t, v)
	g := &fixtureGenerator{id: "generation-fixture-v1"}
	configureGeneration(t, f.service, g)
	return f, g, v
}
func configureGeneration(t *testing.T, s *Service, g *fixtureGenerator) {
	t.Helper()
	err := s.ConfigureGeneration(context.Background(), g, skillmaintenance.Policy{MaxDailyModelCalls: 30, MaxDailyTokens: 100000, MaxJobSeconds: 300, Workers: 1}, []string{"project"}, false)
	if err != nil {
		t.Fatal(err)
	}
}
func queueGeneration(t *testing.T, f *fixture, key string) harness.SkillGenerationJob {
	t.Helper()
	j, e := f.service.EnqueueGeneration(context.Background(), harness.SkillGenerationRequest{SourceID: "project", Name: "research", RequestKey: key, Reason: "Please verify sources", Base: f.head(t)})
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func generationRun(t *testing.T, s *Service) {
	t.Helper()
	worked, e := s.RunGeneration(context.Background())
	if e != nil || !worked {
		t.Fatalf("run %v %v", worked, e)
	}
}

func TestGenerationOperatorPreparationIsReadOnlyAndIdentityBound(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	status := f.service.GenerationStatus()
	if !status.Enabled || status.Version != "skill-generation-v1" || len(status.Sources) != 1 || status.Sources[0].SourceID != "project" || status.Sources[0].Name != "" || status.Budget == nil || status.Budget.MaxDailyModelCalls != 30 {
		t.Fatalf("status: %+v", status)
	}
	target := status.Sources[0]
	target.Name = "new-skill"
	prepared, err := f.service.PrepareGeneration(ctx, target)
	if err != nil || prepared.Target != target || prepared.Base != (harness.SkillHead{}) || prepared.OverlayBase != nil {
		t.Fatalf("absent target: %+v %v", prepared, err)
	}
	target.Name = "research"
	prepared, err = f.service.PrepareGeneration(ctx, target)
	if err != nil || prepared.Base != f.head(t) {
		t.Fatalf("existing target: %+v %v", prepared, err)
	}
	for _, wrong := range []harness.SkillTarget{
		{SourceID: target.SourceID, Name: target.Name, SourceIdentity: strings.Repeat("b", 64), Scope: target.Scope, Workspace: target.Workspace},
		{SourceID: target.SourceID, Name: target.Name, SourceIdentity: target.SourceIdentity, Scope: target.Scope, Workspace: target.Workspace + "-rebound"},
	} {
		if _, err := f.service.PrepareGeneration(ctx, wrong); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatal("unobserved target accepted", err)
		}
	}
	status.Sources[0].SourceID = "changed"
	status.Budget.MaxDailyModelCalls = 100000
	if next := f.service.GenerationStatus(); next.Sources[0].SourceID != "project" || next.Budget.MaxDailyModelCalls != 30 {
		t.Fatal("status exposed mutable grants")
	}
	page, err := f.service.QueryGeneration(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Jobs) != 0 || g.calls.Load() != 0 || v.calls.Load() != 0 {
		t.Fatal("read-only preparation queued model work", err)
	}
}

func TestGenerationPreparationRequiresBuiltinGrantAndImportedOrigin(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	f.cfg.BuiltinSources = []string{"project"}
	reopenBuiltin(t, f)
	policy := skillmaintenance.Policy{MaxDailyModelCalls: 30, MaxDailyTokens: 100000, MaxJobSeconds: 300, Workers: 1}
	if err := f.service.ConfigureGeneration(ctx, g, policy, []string{"project"}, false); err == nil {
		t.Fatal("builtin generation enabled without overlay grant")
	}
	if status := f.service.GenerationStatus(); status.Enabled || len(status.Sources) != 0 || status.Budget != nil {
		t.Fatalf("failed configuration exposed generation authority: %+v", status)
	}
	if err := f.service.ConfigureGeneration(ctx, g, policy, []string{"project"}, true); err != nil {
		t.Fatal(err)
	}
	target := f.service.GenerationStatus().Sources[0]
	target.Name = "research"
	base := f.head(t)
	prepared, err := f.service.PrepareGeneration(ctx, target)
	want := harness.SkillRef{SourceID: target.SourceID, SourceIdentity: target.SourceIdentity, Name: target.Name, Version: base.Version, Hash: base.Version}
	if err != nil || prepared.Base != base || prepared.OverlayBase == nil || *prepared.OverlayBase != want {
		t.Fatalf("builtin preparation: %+v %v", prepared, err)
	}
	target.Name = "new-skill"
	if _, err := f.service.PrepareGeneration(ctx, target); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("creation in builtin source was not denied: %v", err)
	}
	page, err := f.service.QueryGeneration(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Jobs) != 0 || g.calls.Load() != 0 || v.calls.Load() != 0 || f.head(t) != base {
		t.Fatal("preparation changed published state or queued model work", err)
	}
	var overlays int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_overlays`).Scan(&overlays); err != nil || overlays != 0 {
		t.Fatalf("preparation activated overlay: %d %v", overlays, err)
	}
}

func TestGenerationQueueReviewAndPublishAreSeparate(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	base := f.head(t)
	job := queueGeneration(t, f, "edit")
	if g.calls.Load() != 0 || v.calls.Load() != 0 {
		t.Fatal("enqueue called a model")
	}
	duplicate := queueGeneration(t, f, "edit")
	if duplicate.ID != job.ID {
		t.Fatal("enqueue was not idempotent")
	}
	generationRun(t, f.service)
	finished, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || finished.State != "completed" || finished.ProposalID == "" || finished.CandidateVersion == "" {
		t.Fatalf("job %+v %v", finished, e)
	}
	p, e := f.service.Get(ctx, finished.ProposalID)
	if e != nil || p.Status != "pending_review" || p.Origin != "automatic" || p.Trigger.Type != "operator_generation" {
		t.Fatalf("proposal %+v %v", p, e)
	}
	if f.head(t) != base || g.calls.Load() != 1 || v.calls.Load() != 1 {
		t.Fatal("generation published or replayed work")
	}
	receipts, e := f.service.GenerationReceipts(ctx, job.ID, 0, 100)
	if e != nil || len(receipts) != 3 {
		t.Fatalf("receipts %+v %v", receipts, e)
	}
	for i, stage := range []string{"generation", "security", "quality"} {
		if receipts[i].Stage != stage || receipts[i].ChargedTokens != 15 {
			t.Fatal(receipts)
		}
	}
	if _, e = f.service.Validate(ctx, p.ID, p.StateVersion); !errors.Is(e, harness.ErrPermissionDenied) {
		t.Fatalf("unbudgeted review allowed %v", e)
	}
	if _, e = f.service.Approve(ctx, p.ID, review(p)); e != nil {
		t.Fatal(e)
	}
	if f.head(t).Version != p.CandidateVersion {
		t.Fatal("operator approval did not publish")
	}
	if e = f.service.CancelGeneration(ctx, job.ID, finished.StateVersion); !errors.Is(e, harness.ErrSkillConflict) {
		t.Fatalf("cancel published %v", e)
	}
	// A duplicate after publication returns the original job instead of charging.
	again, e := f.service.EnqueueGeneration(ctx, harness.SkillGenerationRequest{SourceID: "project", Name: "research", RequestKey: "edit", Reason: "Please verify sources", Base: base})
	if e != nil || again.ID != job.ID {
		t.Fatalf("late duplicate %+v %v", again, e)
	}
}

func TestGenerationRestartsFromSavedCandidateAndProposal(t *testing.T) {
	for _, phase := range []string{"candidate", "proposal"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			f, g, v := generationFixture(t)
			job := queueGeneration(t, f, "resume")
			claim, ok, e := f.service.generation.store.Claim(ctx)
			if e != nil || !ok {
				t.Fatalf("claim %v %v", ok, e)
			}
			work := context.WithValue(skillmaintenance.WithBroker(ctx, f.service.generation.store.Broker(claim)), generationContextKey{}, job.ID)
			_, input, e := f.service.generationInput(work, job.ID)
			if e != nil {
				t.Fatal(e)
			}
			candidate, e := g.Generate(work, input)
			if e != nil {
				t.Fatal(e)
			}
			if e = f.service.saveGeneratedCandidate(work, job.ID, candidate); e != nil {
				t.Fatal(e)
			}
			var p Proposal
			if phase == "proposal" {
				p, e = f.service.submit(work, Request{SourceID: "project", Name: "research", RequestKey: "generation/" + job.ID, Action: "edit", Reason: candidate.Reason, Author: "operator", Origin: "automatic", Base: job.Base, Files: candidate.Files, Trigger: Trigger{Type: "operator_generation", Summary: "Please verify sources"}}, false)
				if e != nil || p.Status != "validating" {
					t.Fatalf("stage %+v %v", p, e)
				}
				if _, e = f.service.Validate(ctx, p.ID, p.StateVersion); !errors.Is(e, harness.ErrPermissionDenied) {
					t.Fatalf("review bypass %v", e)
				}
			}
			// Simulate the old process losing its lease, without changing deadline,
			// input, receipts or attempts; a new service must consume the checkpoint.
			if _, e = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET lease_ns=0 WHERE id=?`, job.ID); e != nil {
				t.Fatal(e)
			}
			next, e := New(ctx, f.registry, v)
			if e != nil {
				t.Fatal(e)
			}
			configureGeneration(t, next, g)
			generationRun(t, next)
			finished, e := next.GetGeneration(ctx, job.ID)
			if e != nil || finished.State != "completed" || finished.Attempts != 2 || p.ID != "" && finished.ProposalID != p.ID {
				t.Fatalf("recovered %+v %v", finished, e)
			}
			if g.calls.Load() != 1 || v.calls.Load() != 1 {
				t.Fatalf("replayed generation %d review %d", g.calls.Load(), v.calls.Load())
			}
			if e = f.service.saveGeneratedCandidate(work, job.ID, candidate); !errors.Is(e, skillmaintenance.ErrConflict) {
				t.Fatalf("stale commit %v", e)
			}
			receipts, e := next.GenerationReceipts(ctx, job.ID, 0, 100)
			if e != nil || len(receipts) != 3 || receipts[0].Attempt != 1 || receipts[1].Attempt != 2 {
				t.Fatalf("receipt history %+v %v", receipts, e)
			}
		})
	}
}

func TestGenerationTransactionsRejectOrphansAndInvalidCandidates(t *testing.T) {
	ctx := context.Background()
	f, g, _ := generationFixture(t)
	_, e := f.db.DB().Exec(`CREATE TRIGGER fail_generation_input BEFORE INSERT ON df_skill_generation_inputs BEGIN SELECT RAISE(ABORT,'private-storage-error'); END`)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.service.EnqueueGeneration(ctx, harness.SkillGenerationRequest{SourceID: "project", Name: "research", RequestKey: "fault", Reason: "Verify", Base: f.head(t)})
	if e == nil {
		t.Fatal("expected atomic insert failure")
	}
	var count int
	if e = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_jobs`).Scan(&count); e != nil || count != 0 {
		t.Fatalf("orphan jobs %d %v", count, e)
	}
	if _, e = f.db.DB().Exec(`DROP TRIGGER fail_generation_input`); e != nil {
		t.Fatal(e)
	}
	g.body = func(context.Context, skillmaintenance.GenerationInput) (skillmaintenance.Candidate, error) {
		files := skillFiles("Use the helper")
		files["helper.pyc"] = []byte("unsupported bytecode")
		return skillmaintenance.Candidate{Reason: "Add helper", Files: files}, nil
	}
	job := queueGeneration(t, f, "python")
	generationRun(t, f.service)
	failed, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || failed.State != "failed" || failed.ErrorCode != "invalid_candidate" || failed.ProposalID != "" || failed.CandidateVersion != "" {
		t.Fatalf("invalid candidate %+v %v", failed, e)
	}
	if e = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_versions`).Scan(&count); e != nil || count != 1 {
		t.Fatalf("invalid stage retained %d %v", count, e)
	}
}

func TestGenerationStaleBaseAndConfigurationDoNotCallModels(t *testing.T) {
	for _, mode := range []string{"base", "model", "source"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, g, v := generationFixture(t)
			job := queueGeneration(t, f, "policy")
			s := f.service
			switch mode {
			case "base":
				_, e := f.registry.SetEnabledChecked(ctx, harness.SkillEnablement{Target: job.Target, Expected: job.Base, Enabled: false, RequestKey: "disable", Actor: "operator"})
				if e != nil {
					t.Fatal(e)
				}
			case "model":
				g = &fixtureGenerator{id: "changed-model"}
				var e error
				s, e = New(ctx, f.registry, v)
				if e != nil {
					t.Fatal(e)
				}
				configureGeneration(t, s, g)
			case "source":
				// The host's immutable grant is absent for this queued source. This
				// mirrors a restarted host configured with a different allowlist.
				s.generation.allowed = map[string]harness.SkillTarget{}
			}
			generationRun(t, s)
			result, e := s.GetGeneration(ctx, job.ID)
			if e != nil || result.State != "failed" {
				t.Fatalf("rejected %+v %v", result, e)
			}
			code := map[string]string{"base": "base_changed", "model": "configuration_changed", "source": "source_unavailable"}[mode]
			if result.ErrorCode != code || g.calls.Load() != 0 || v.calls.Load() != 0 {
				t.Fatalf("policy calls %+v gen=%d review=%d", result, g.calls.Load(), v.calls.Load())
			}
		})
	}
}

func TestGenerationCancellationAndPagination(t *testing.T) {
	ctx := context.Background()
	f, g, _ := generationFixture(t)
	first := queueGeneration(t, f, "first")
	queueGeneration(t, f, "second")
	page, e := f.service.QueryGeneration(ctx, harness.SkillQuery{Limit: 1})
	if e != nil || len(page.Jobs) != 1 || page.Next == "" {
		t.Fatalf("page %+v %v", page, e)
	}
	if e = f.service.CancelGeneration(ctx, first.ID, first.StateVersion); e != nil {
		t.Fatal(e)
	}
	if e = f.service.CancelGeneration(ctx, first.ID, first.StateVersion); !errors.Is(e, skillmaintenance.ErrConflict) {
		t.Fatal(e)
	}
	if _, e = f.service.QueryGeneration(ctx, harness.SkillQuery{Limit: 1, Cursor: page.Next}); !errors.Is(e, harness.ErrSkillConflict) {
		t.Fatalf("stale page accepted %v", e)
	}
	generationRun(t, f.service)
	if g.calls.Load() != 1 {
		t.Fatal("cancelled job ran")
	}
	// Generation can be disabled after restart while history and cancellation
	// remain available to the currently configured source's operator.
	disabled, e := New(ctx, f.registry, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = disabled.GetGeneration(ctx, first.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = disabled.EnqueueGeneration(ctx, harness.SkillGenerationRequest{}); !errors.Is(e, harness.ErrInvalidInput) {
		t.Fatal(e)
	}
}

func TestGenerationReviewRetryPreservesCandidate(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	v.failures.Store(1)
	job := queueGeneration(t, f, "review-retry")
	generationRun(t, f.service)
	first, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || first.State != "retry" || first.ProposalID == "" {
		t.Fatalf("retry %+v %v", first, e)
	}
	p, e := f.service.Get(ctx, first.ProposalID)
	if e != nil || p.Status != "validating" {
		t.Fatalf("retry proposal %+v %v", p, e)
	}
	if _, e = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, job.ID); e != nil {
		t.Fatal(e)
	}
	generationRun(t, f.service)
	final, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || final.State != "completed" || final.Attempts != 2 || final.ProposalID != first.ProposalID || final.CandidateVersion != first.CandidateVersion {
		t.Fatalf("final %+v %v", final, e)
	}
	if g.calls.Load() != 1 || v.calls.Load() != 2 {
		t.Fatalf("replayed generation=%d reviews=%d", g.calls.Load(), v.calls.Load())
	}
	receipts, e := f.service.GenerationReceipts(ctx, job.ID, 0, 100)
	if e != nil || len(receipts) != 5 {
		t.Fatalf("lost retry costs %+v %v", receipts, e)
	}
}

func TestGenerationCancelDuringReviewRejectsProposalAtomically(t *testing.T) {
	ctx := context.Background()
	f, _, v := generationFixture(t)
	job := queueGeneration(t, f, "cancel-review")
	started, release := make(chan struct{}), make(chan struct{})
	v.before = func(context.Context) { close(started); <-release }
	done := make(chan error, 1)
	go func() { _, e := f.service.RunGeneration(ctx); done <- e }()
	<-started
	current, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || current.ProposalID == "" {
		close(release)
		t.Fatalf("running %+v %v", current, e)
	}
	if e = f.service.CancelGeneration(ctx, job.ID, current.StateVersion); e != nil {
		close(release)
		t.Fatal(e)
	}
	p, e := f.service.Get(ctx, current.ProposalID)
	if e != nil || p.Status != "rejected" {
		close(release)
		t.Fatalf("cancel not atomic %+v %v", p, e)
	}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	r, e := f.service.GenerationReceipts(ctx, job.ID, 0, 100)
	if e != nil || len(r) != 1 || r[0].Stage != "generation" {
		t.Fatalf("cancel admitted review %+v %v", r, e)
	}
	if _, e = f.service.Approve(ctx, p.ID, review(p)); !errors.Is(e, harness.ErrSkillConflict) {
		t.Fatalf("cancelled proposal approved %v", e)
	}
}

func TestGenerationCommittedReviewRecoversWithoutNewModelConfiguration(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	job := queueGeneration(t, f, "finish-fault")
	if _, e := f.db.DB().Exec(`CREATE TRIGGER fail_generation_finish BEFORE UPDATE ON df_skill_maintenance_jobs WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'fixture-finish-fault'); END`); e != nil {
		t.Fatal(e)
	}
	if _, e := f.service.RunGeneration(ctx); e == nil {
		t.Fatal("expected final commit fault")
	}
	saved, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || saved.State != "running" || saved.ProposalID == "" {
		t.Fatalf("lost review checkpoint %+v %v", saved, e)
	}
	p, e := f.service.Get(ctx, saved.ProposalID)
	if e != nil || p.Status != "pending_review" {
		t.Fatalf("review checkpoint %+v %v", p, e)
	}
	if _, e = f.service.Approve(ctx, p.ID, review(p)); e != nil {
		t.Fatal(e)
	}
	if _, e = f.db.DB().Exec(`DROP TRIGGER fail_generation_finish`); e != nil {
		t.Fatal(e)
	}
	if _, e = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET lease_ns=0 WHERE id=?`, job.ID); e != nil {
		t.Fatal(e)
	}
	nextGenerator := &fixtureGenerator{id: "new-generation-model"}
	nextValidator := &fixtureGenerationValidator{id: "new-review-model"}
	next, e := New(ctx, f.registry, nextValidator)
	if e != nil {
		t.Fatal(e)
	}
	configureGeneration(t, next, nextGenerator)
	generationRun(t, next)
	finished, e := next.GetGeneration(ctx, job.ID)
	if e != nil || finished.State != "completed" || finished.ProposalID != p.ID {
		t.Fatalf("ack %+v %v", finished, e)
	}
	if g.calls.Load() != 1 || v.calls.Load() != 1 || nextGenerator.calls.Load() != 0 || nextValidator.calls.Load() != 0 {
		t.Fatal("committed review was regenerated or reevaluated")
	}
	if f.head(t).Version != p.CandidateVersion {
		t.Fatal("recovery changed published head")
	}
}

func TestGenerationSourceIdentityRevocationHidesHistoryAndStopsDispatch(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	job := queueGeneration(t, f, "revoked-root")
	workspace := t.TempDir()
	root := filepath.Join(workspace, "replacement")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	registry, e := skills.NewRegistry(ctx, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Workspace: workspace, Scope: harness.SkillScopeWorkspace}}}, f.db.DB())
	if e != nil {
		t.Fatal(e)
	}
	defer registry.Close()
	next, e := New(ctx, registry, v)
	if e != nil {
		t.Fatal(e)
	}
	configureGeneration(t, next, g)
	if _, e = next.GetGeneration(ctx, job.ID); !errors.Is(e, harness.ErrPermissionDenied) {
		t.Fatalf("old scope visible %v", e)
	}
	if _, e = next.GenerationReceipts(ctx, job.ID, 0, 100); !errors.Is(e, harness.ErrPermissionDenied) {
		t.Fatalf("old receipts visible %v", e)
	}
	page, e := next.QueryGeneration(ctx, harness.SkillQuery{Limit: 10})
	if e != nil || len(page.Jobs) != 0 {
		t.Fatalf("old scope listed %+v %v", page, e)
	}
	generationRun(t, next)
	stored, e := readGeneration(ctx, f.db.DB(), job.ID)
	if e != nil || stored.job.State != "failed" || stored.job.ErrorCode != "source_unavailable" || g.calls.Load() != 0 || v.calls.Load() != 0 {
		t.Fatalf("revoked job dispatched %+v %v", stored.job, e)
	}
}

func TestGenerationCandidateCheckpointRollsBackWithProgress(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	job := queueGeneration(t, f, "candidate-fault")
	if _, e := f.db.DB().Exec(`CREATE TRIGGER fail_candidate_progress BEFORE UPDATE ON df_skill_generation_inputs WHEN NEW.candidate_version<>'' BEGIN SELECT RAISE(ABORT,'candidate-progress-fault'); END`); e != nil {
		t.Fatal(e)
	}
	generationRun(t, f.service)
	interrupted, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || interrupted.State != "retry" || interrupted.CandidateVersion != "" || interrupted.ErrorCode != "storage_error" {
		t.Fatalf("progress %+v %v", interrupted, e)
	}
	var versions int
	if e = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_versions`).Scan(&versions); e != nil || versions != 1 {
		t.Fatalf("orphan version %d %v", versions, e)
	}
	if _, e = f.db.DB().Exec(`DROP TRIGGER fail_candidate_progress`); e != nil {
		t.Fatal(e)
	}
	if _, e = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, job.ID); e != nil {
		t.Fatal(e)
	}
	generationRun(t, f.service)
	finished, e := f.service.GetGeneration(ctx, job.ID)
	if e != nil || finished.State != "completed" || g.calls.Load() != 2 || v.calls.Load() != 1 {
		t.Fatalf("retry %+v %v", finished, e)
	}
	receipts, e := f.service.GenerationReceipts(ctx, job.ID, 0, 100)
	if e != nil || len(receipts) != 4 {
		t.Fatalf("lost failed attempt charge %+v %v", receipts, e)
	}
}
