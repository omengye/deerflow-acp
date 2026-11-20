package evolution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func TestSafetyPublicationCreatesAndUpdatesWithoutUsageEvidence(t *testing.T) {
	for _, action := range []string{"create", "edit"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			f := newSubmittedNativeFixture(t)
			// No behavior evaluator, automatic patch service or sandbox is needed.
			f.service.behavior, f.service.generation.behavior, f.service.autoPatch = nil, nil, nil
			run := f.start(t, f.cfg.Sources[0].Workspace, "Write reusable instructions for this task.")
			req := f.request(t, "safe-"+action, "Use the checked inputs")
			if action == "create" {
				req.Action, req.Name, req.Base = "create", "fresh-skill", harness.SkillHead{}
				req.Files["SKILL.md"] = []byte("---\nname: fresh-skill\ndescription: Check sample input\n---\nUse the checked inputs.\n")
			}
			req.Files["scripts/check.py"] = []byte("raise SystemExit(99)\n")
			req.Trigger = Trigger{Type: "explicit_agent_proposal", ThreadID: run.Session.ID, RunID: run.RunID}
			p, err := f.service.SubmitAutomatic(ctx, req, run.Session.CWD)
			if err != nil || p.PublishedRevision != 0 {
				t.Fatal(p, err)
			}
			f.finish(t, run, false)
			generationRun(t, f.service)
			done, progress, err := f.service.CandidateSnapshot(ctx, p.ID)
			if err != nil || done.Status != "published" || done.Approval == nil || done.Approval.SecurityReview == nil || done.Approval.UserConfirmation != nil ||
				done.FilePublicationPending || done.Evaluation.Security != "passed" || done.Evaluation.Behavior != "not_run" || done.Evaluation.IsolatedScripts || done.ScriptReviewRequired ||
				progress.Status != "published" || progress.Evidence != nil || progress.Observation != nil || f.g.calls.Load() != 0 || f.e.calls.Load() != 0 || f.v.calls.Load() != 1 {
				t.Fatal("incorrect safety publication or receipt", done.Status, done.Error, progress, err)
			}
			sel := harness.SkillSelection{Workspace: run.Session.CWD}
			if err := f.registry.RefreshSources(ctx, sel); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.registry.Snapshot(ctx, sel)
			if err != nil {
				t.Fatal(err)
			}
			_, err = snapshot.Get(ctx, req.Name)
			if err != nil || !slices.ContainsFunc(snapshot.Refs(), func(ref harness.SkillRef) bool {
				return ref.Name == req.Name && ref.Version == p.CandidateVersion && ref.SourceIdentity == p.Target.SourceIdentity
			}) {
				t.Fatal("installed version unavailable", err)
			}
			script, err := os.ReadFile(filepath.Join(f.cfg.Sources[0].Root, req.Name, "scripts", "check.py"))
			if err != nil || string(script) != string(req.Files["scripts/check.py"]) {
				t.Fatal("script not installed", err)
			}
			job, err := skillmaintenance.ReadJob(ctx, f.db.DB(), p.GenerationJobID)
			if err != nil || job.State != "completed" {
				t.Fatal(job, err)
			}
			for range 2 {
				if _, err = f.service.RetryPublication(ctx, p.ID, done.Approval.Review.ExpectedStateVersion); err != nil {
					t.Fatal(err)
				}
			}
			var revisions, observations int
			if err = f.db.DB().QueryRow(`SELECT (SELECT count(*) FROM df_skill_publications WHERE request_key=?),(SELECT count(*) FROM df_skill_auto_patch_observations WHERE proposal_id=?)`, done.Approval.FilePublicationKey(), p.ID).Scan(&revisions, &observations); err != nil || revisions != 1 || observations != 0 {
				t.Fatal(revisions, observations, err)
			}
		})
	}
}

func TestSecurityPublicationUpgradesBackgroundJobs(t *testing.T) {
	for _, withCandidate := range []bool{false, true} {
		name := "awaiting_generation"
		if withCandidate {
			name = "already_reviewed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, g, v := generationFixture(t)
			job := queueGeneration(t, f, "upgrade")
			var previous Proposal
			if withCandidate {
				generationRun(t, f.service)
				legacy, err := f.service.GetGeneration(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				previous, err = f.service.Get(ctx, legacy.ProposalID)
				if err != nil || previous.Status != "pending_review" {
					t.Fatal(previous, err)
				}
			}
			next, err := New(ctx, f.registry, v)
			if err != nil {
				t.Fatal(err)
			}
			configureGeneration(t, next, g)
			if err := next.ConfigureSecurityPublication(ctx); err != nil {
				t.Fatal(err)
			}
			generationRun(t, next)
			job, err = next.GetGeneration(ctx, job.ID)
			if err != nil || job.State != "completed" {
				t.Fatal(job, err)
			}
			p, err := next.Get(ctx, job.ProposalID)
			if err != nil || p.Status != "published" || p.Evaluation.Security != "passed" || p.GenerationJobID != job.ID || p.BehaviorJobID != "" || g.calls.Load() != 1 {
				t.Fatal("background upgrade failed", p, err)
			}
			if withCandidate && (p.ID != previous.ID || p.CandidateVersion != previous.CandidateVersion) {
				t.Fatal("replaced saved candidate")
			}
			if err = next.upgradeSecurityReviews(ctx); err != nil {
				t.Fatal(err)
			}
			if worked, err := next.RunGeneration(ctx); worked || err != nil {
				t.Fatal("replayed publication", worked, err)
			}
		})
	}
}

func TestSecurityPublicationOfOperatorPackageInGlobalSource(t *testing.T) {
	ctx := context.Background()
	v := &fixtureGenerationValidator{id: "security"}
	f := newFixtureWithScope(t, v, harness.SkillScopeGlobal)
	g := &fixtureGenerator{id: "generator"}
	configureGeneration(t, f.service, g)
	if err := f.service.ConfigureSecurityPublication(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := f.service.Submit(ctx, f.request(t, "operator-update", "Updated global instructions"))
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "published" || p.Approval == nil || p.Approval.SecurityReview == nil || p.Approval.SecurityReview.WorkspaceOnly || p.Approval.SecurityReview.Workspace != "" || g.calls.Load() != 0 || v.calls.Load() != 1 {
		t.Fatal(p, err)
	}
	data, err := os.ReadFile(filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md"))
	if err != nil || string(data) != string(skillFiles("Updated global instructions")["SKILL.md"]) {
		t.Fatal("global file not installed", err)
	}
	if err = f.registry.SetEnabled(ctx, "project", "research", false); err != nil {
		t.Fatal(err)
	}
	p, err = f.service.Submit(ctx, f.request(t, "update-disabled", "Revised disabled instructions"))
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "published" || f.head(t).Enabled || f.head(t).Version != p.CandidateVersion {
		t.Fatal("updating a disabled skill should preserve its enablement", p, err)
	}
}

func TestDiscoveryRevokedDuringSecurityReviewCannotPublish(t *testing.T) {
	ctx := context.Background()
	f, _, v := discoveryFixture(t)
	if err := f.service.ConfigureSecurityPublication(ctx); err != nil {
		t.Fatal(err)
	}
	first := discoveryCorrection(t, f, "", "one")
	discoveryCorrection(t, f, "", "two")
	discoverySteps(t, f.service, 2)
	d := discoveries(t, f.service)[0]
	base := f.head(t)
	v.before = func(context.Context) { withdrawDiscoveryFeedback(t, f.service, first.SignalID) }
	generationRun(t, f.service)
	j, err := f.service.GetGeneration(ctx, d.GenerationID)
	if err != nil || j.State != "failed" || j.ErrorCode != "evidence_changed" {
		t.Fatal(j, err)
	}
	p, err := f.service.Get(ctx, j.ProposalID)
	if err != nil || p.Status != "stale" || p.Approval != nil || f.head(t) != base {
		t.Fatal(p, err)
	}
}

func TestSafetyPublicationRejectsInvalidSafetyOrAuthority(t *testing.T) {
	for _, mode := range []string{"unsafe", "unavailable", "legacy_fixture_wait", "wrong_candidate", "conflicting_check", "changed_source_input", "retired", "changed_files", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newSubmittedNativeFixture(t)
			p, origin := f.submit(t)
			f.service.validator = validatorFunc(func(_ context.Context, input ValidationInput) (Report, error) {
				report := Report{ValidatorVersion: f.v.id, BaseVersion: input.Proposal.Base.Version, CandidateVersion: input.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}
				switch mode {
				case "unsafe":
					report.Security = "failed"
				case "unavailable":
					report.Security = "unavailable"
				case "legacy_fixture_wait":
					return report, skillmaintenance.ErrBlockedEvidence
				case "wrong_candidate":
					report.CandidateVersion = "wrong"
				case "conflicting_check":
					report.ModelChecks = []harness.SkillModelCheck{{Stage: "security", Passed: false}}
				}
				return report, nil
			})
			switch mode {
			case "retired":
				_, err := f.db.DB().Exec(`INSERT INTO df_evolution_retired_proposals VALUES(?)`, p.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "changed_source_input":
				if _, err := f.db.DB().Exec(`UPDATE harness_inputs SET content=? WHERE run_id=?`, []byte("different"), origin.RunID); err != nil {
					t.Fatal(err)
				}
			case "changed_files":
				if err := os.WriteFile(filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md"), skillFiles("Concurrent edit")["SKILL.md"], 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if _, err := f.service.Reject(ctx, p.ID, review(p)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.service.RunGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.service.Get(ctx, p.ID)
			if err != nil || got.Approval != nil || got.PublishedRevision != 0 || got.Status == "published" || f.head(t).Version == p.CandidateVersion {
				t.Fatal("unreviewed or conflicting package installed", got.Status, err)
			}
			if mode == "legacy_fixture_wait" && got.Status != "failed" {
				t.Fatal("an incompatible validator left the candidate waiting for evidence", got.Status)
			}
			if mode == "changed_files" && got.Status != "stale" {
				t.Fatal(got.Status)
			}
			if _, err = f.service.RetryPublication(ctx, p.ID, p.StateVersion); !errors.Is(err, ErrConflict) {
				t.Fatal("unapproved publication could be resumed", err)
			}
		})
	}
}

func TestSafetyPublicationFailureIsAtomicAndJournalRecovers(t *testing.T) {
	for _, fault := range []string{"before_commit", "after_files"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			f := newSubmittedNativeFixture(t)
			p, _ := f.submit(t)
			statement := `CREATE TRIGGER publication_fault BEFORE INSERT ON df_skill_publications WHEN NEW.request_key LIKE 'security/%' BEGIN SELECT RAISE(ABORT,'publication fault'); END`
			if fault == "after_files" {
				statement = `CREATE TRIGGER publication_fault BEFORE UPDATE OF status ON df_skill_proposals WHEN NEW.status='published' BEGIN SELECT RAISE(ABORT,'finalization fault'); END`
			}
			if _, err := f.db.DB().Exec(statement); err != nil {
				t.Fatal(err)
			}
			generationRun(t, f.service)
			saved, err := f.service.Get(ctx, p.ID)
			if err != nil || saved.Evaluation.Security != "passed" || f.head(t) != p.Base {
				t.Fatal(saved, err)
			}
			if fault == "before_commit" && (saved.Approval != nil || saved.Status != "pending_review") {
				t.Fatal("uncommitted decision leaked", saved.Status)
			}
			if fault == "after_files" && (saved.Approval == nil || saved.Status != "publishing") {
				t.Fatal("lost durable decision", saved.Status)
			}
			if _, err = f.db.DB().Exec(`DROP TRIGGER publication_fault`); err != nil {
				t.Fatal(err)
			}
			if _, err = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=? AND state='retry'`, p.GenerationJobID); err != nil {
				t.Fatal(err)
			}
			generationRun(t, f.service)
			saved, err = f.service.Get(ctx, p.ID)
			if err != nil || saved.Status != "published" || f.head(t).Version != p.CandidateVersion || f.v.calls.Load() != 1 {
				t.Fatal("failed to recover the exact reviewed bytes", saved.Status, saved.Error, err)
			}
			var count int
			if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_publications WHERE request_key=?`, "security/"+p.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestSecurityPublicationClosesLegacySampling(t *testing.T) {
	for _, route := range []string{"observing", "quarantined"} {
		t.Run(route, func(t *testing.T) {
			ctx := context.Background()
			f, g, v := generationFixture(t)
			suite := behaviorSuite()
			regression := suite.Cases[0]
			regression.ID, regression.Kind, regression.Holdout = "regression", "regression", true
			suite.Cases = append(suite.Cases, regression)
			e := &fixtureBehaviorEvaluator{id: "legacy", result: func(in harness.SkillBehaviorInput) (bool, error) {
				return in.Variant != "base" || in.Case.Kind != "targeted", nil
			}}
			configureBehavior(t, f.service, e, suite)
			if err := f.service.ConfigureAutoPatch(harness.SkillAutoPatchOptions{MaxChangedLines: 200, ObservationHours: 72, MinObservations: 5, RollbackFailureThreshold: 2}); err != nil {
				t.Fatal(err)
			}
			job := queueGeneration(t, f, "legacy-observation")
			generationRun(t, f.service)
			if err := f.service.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			job, err := f.service.GetGeneration(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			old, err := readAutoPatchObservation(ctx, f.db.DB(), job.ProposalID)
			if err != nil || old.Status != "observing" {
				t.Fatal(old, err)
			}
			if _, err := f.db.DB().Exec(`UPDATE df_skill_routes SET state=? WHERE source_key=? AND name=? AND candidate_version=?`, route, old.Target.SourceIdentity, old.Target.Name, old.Head.Version); err != nil {
				t.Fatal(err)
			}
			next, err := New(ctx, f.registry, v)
			if err != nil {
				t.Fatal(err)
			}
			configureGeneration(t, next, g)
			if err := next.ConfigureSecurityPublication(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := readAutoPatchObservation(ctx, f.db.DB(), job.ProposalID)
			want := "stable"
			if route == "quarantined" {
				want = "withdrawn"
			}
			if err != nil || got.Status != want || got.Successes != old.Successes || got.PublicationRevision != old.PublicationRevision || f.head(t) != old.Head {
				t.Fatal(got, err)
			}
			if worked, err := next.RunGeneration(ctx); worked || err != nil {
				t.Fatal("scheduled a historical behavior evaluation", worked, err)
			}
		})
	}
}
