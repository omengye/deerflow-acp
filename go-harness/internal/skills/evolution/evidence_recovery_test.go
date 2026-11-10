package evolution

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func TestWaitingCandidateRebindsNewHostSuiteWithoutResubmission(t *testing.T) {
	ctx := context.Background()
	f, g, v, e := submittedFixture(t, false)
	p, err := f.service.SubmitAutomatic(ctx, f.request(t, "later-suite", "Verify sources before summarizing"), f.cfg.Sources[0].Workspace)
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	before, err := f.service.CandidateProgress(ctx, p.ID)
	if err != nil || before.Status != "blocked_evidence" || before.Evidence == nil ||
		!slices.Contains(before.Evidence.Missing, "source_task_binding") {
		t.Fatal("missing evidence diagnostics", before, err)
	}
	old, err := readBehavior(ctx, f.db.DB(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A normal host restart loads an independently maintained matching suite.
	next, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	configureGeneration(t, next, g)
	suite := behaviorSuite()
	holdout := suite.Cases[0]
	holdout.ID, holdout.Kind, holdout.Holdout = "independent", "regression", true
	suite.Cases = append(suite.Cases, holdout)
	configureBehavior(t, next, e, suite)
	if err = next.ConfigureAutoPatch(harness.SkillAutoPatchOptions{MaxChangedLines: 40, ObservationHours: 72, MinObservations: 5, RollbackFailureThreshold: 2}); err != nil {
		t.Fatal(err)
	}
	f.service = next
	generationRun(t, next)
	if err = next.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	done, err := next.Get(ctx, p.ID)
	if err != nil || done.Status != "published" || done.CandidateVersion != p.CandidateVersion {
		t.Fatal("original candidate did not resume", done.Status, done.Error, err)
	}
	if g.calls.Load() != 0 || v.calls.Load() != 1 || e.calls.Load() != 4 {
		t.Fatal("replayed generation/content or skipped comparison", g.calls.Load(), v.calls.Load(), e.calls.Load())
	}
	job, err := skillmaintenance.ReadJob(ctx, f.db.DB(), old.jobID)
	if err != nil || job.State != "failed" || job.ErrorCode != "configuration_changed" {
		t.Fatal("obsolete waiting job remains active", job, err)
	}
	var archived int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_behavior_history WHERE id=?`, old.input.ID).Scan(&archived); err != nil || archived != 1 {
		t.Fatal("lost original audit", archived, err)
	}
	for range 2 {
		next.generation.dependencyProbe = time.Time{}
		if _, err = next.RunGeneration(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if e.calls.Load() != 4 {
		t.Fatal("unchanged environment replayed behavior")
	}
}

func TestWaitingSuiteRecoveryRejectsRegressionAndRetiredCandidates(t *testing.T) {
	for _, mode := range []string{"regression", "retired", "unrelated-suite"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, g, v, e := submittedFixture(t, false)
			p, err := f.service.SubmitAutomatic(ctx, f.request(t, "waiting-"+mode, "Verify sources before summarizing"), f.cfg.Sources[0].Workspace)
			if err != nil {
				t.Fatal(err)
			}
			generationRun(t, f.service)
			if mode == "retired" {
				if _, err = f.db.DB().Exec(`INSERT INTO df_evolution_retired_proposals(proposal_id) VALUES(?)`, p.ID); err != nil {
					t.Fatal(err)
				}
			}
			next, err := New(ctx, f.registry, v)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = next.Close() })
			configureGeneration(t, next, g)
			suite := behaviorSuite()
			if mode == "unrelated-suite" {
				suite.Name = "other"
			}
			e.result = func(in harness.SkillBehaviorInput) (bool, error) { return in.Variant == "base", nil }
			configureBehavior(t, next, e, suite)
			worked, err := next.RunGeneration(ctx)
			if err != nil || worked != (mode == "regression") {
				t.Fatal("unexpected recovery work", worked, err)
			}
			done, err := next.Get(ctx, p.ID)
			if err != nil || f.head(t) != p.Base {
				t.Fatal("recovery changed live head", done, err)
			}
			if mode == "regression" {
				if done.Status != "failed" || e.calls.Load() != 2 {
					t.Fatal("bad candidate passed", done.Status, e.calls.Load())
				}
			} else if done.Status != "validating" || e.calls.Load() != 0 {
				t.Fatal("unrelated or retired candidate resumed", done.Status, e.calls.Load())
			}
		})
	}
}
