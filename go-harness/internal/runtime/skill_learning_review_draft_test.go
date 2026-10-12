package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func enableExperienceReviewDrafts(t *testing.T, e *evolution.Service, newSkill bool) {
	t.Helper()
	o := harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, ExperienceReviewDraftsEnabled: true,
		RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
	if newSkill {
		o.ExperienceNewSkillsEnabled, o.NewSkillSourceID = true, "project"
	}
	if err := e.ConfigureDiscovery(o); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLearningReviewDraftAutomaticSingleEpisodeAndDisabledDiscoveryApproval(t *testing.T) {
	s, x, r, e, g, v, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	// Normal conversation and corrections need neither episode.start nor a
	// manual generation request; all roots still count as one independent task.
	consolidateExtract(t, s, x, "Inspect complete JSON input")
	consolidateRun(t, s)
	consolidateExtract(t, s, x, "Correct the previous check using full text")
	consolidateRun(t, s)
	id := experienceMatch(t, s)
	for _, required := range []int{1, 2} {
		if _, err := s.Store.Learning.Promotion(ctx, id, required, time.Now().Add(-time.Hour), time.Now()); err == nil {
			t.Fatal("automatic publication evidence threshold weakened", required)
		}
	}
	enableExperienceReviewDrafts(t, e, false)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 || !ds[0].ReviewDraft || len(ds[0].Evidence) != 1 || ds[0].HostAdjudicated || ds[0].Attribution != "unassessed" {
		t.Fatal("system did not create one review-only draft", ds)
	}
	p := experienceGenerate(t, e)
	if p.Status != "pending_review" || len(g.seen) != 1 || !g.seen[0].InstructionOnly || len(g.seen[0].Evidence) == 0 || !strings.Contains(g.seen[0].Reason, "always requires human approval") {
		t.Fatal(p, g.seen)
	}
	var consumed, feedback int
	_ = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumed)
	_ = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_feedback`).Scan(&feedback)
	if consumed != 1 || feedback != 0 || len(experienceDiscoveries(t, e)) != 1 {
		t.Fatal("draft fabricated votes or duplicated work", consumed, feedback)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := evolution.New(ctx, r, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if err = next.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = next.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
		t.Fatal(err)
	}
	approved, err := next.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
	if err != nil || approved.Status != "published" || len(g.seen) != 1 {
		t.Fatal("explicit review after restart/disabled discovery failed", approved, err)
	}
}

func TestSkillLearningReviewDraftNeverAutoPublishesButFullEvidenceStillCan(t *testing.T) {
	for _, episodes := range []int{1, 2} {
		t.Run(string(rune('0'+episodes)), func(t *testing.T) {
			s, x, r, e, g, _, _ := experienceDiscoveryTest(t)
			ctx := context.Background()
			adoptLearningBase(t, r)
			g.preserveFrontmatter = true
			evaluator := &autoPatchLearningEvaluator{}
			if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
				t.Fatal(err)
			}
			if episodes == 1 {
				consolidateExtract(t, s, x, "One verified inspection")
				consolidateRun(t, s)
				experienceMatch(t, s)
			} else {
				experienceTwoEpisodes(t, s, x)
			}
			enableExperienceReviewDrafts(t, e, false)
			if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
				t.Fatal(err)
			}
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 || ds[0].ReviewDraft != (episodes == 1) {
				t.Fatal(ds)
			}
			p := autoPatchLearningProposal(t, e, ds[0])
			if p.Evaluation.Behavior != "passed" || evaluator.calls != 4 {
				t.Fatal("fixture did not pass independent evaluation", p, evaluator.calls)
			}
			if err := e.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := e.Get(ctx, p.ID)
			want := "published"
			if episodes == 1 {
				want = "pending_review"
			}
			if err != nil || got.Status != want {
				t.Fatal("review draft gate leaked or full evidence was blocked", got, err)
			}
		})
	}
}

func TestSkillLearningReviewDraftOutputSourceAndAtomicAdmissionGuards(t *testing.T) {
	for _, mode := range []string{"default-off", "already-covered", "ambiguous", "extra-file", "receipt", "withdrawal", "enqueue-fault"} {
		t.Run(mode, func(t *testing.T) {
			s, x, _, e, g, _, matcher := experienceDiscoveryTest(t)
			ctx := context.Background()
			if mode == "already-covered" {
				matcher.relation = "already_covered"
			} else if mode == "ambiguous" {
				matcher.relation = "ambiguous"
			}
			consolidateExtract(t, s, x, "Observe one local procedure")
			consolidateRun(t, s)
			experienceMatch(t, s)
			if mode == "default-off" {
				enableExperienceDiscovery(t, e)
				if ds := experienceDiscoveries(t, e); len(ds) != 0 || len(g.seen) != 0 {
					t.Fatal("review drafts enabled implicitly", ds)
				}
				return
			}
			enableExperienceReviewDrafts(t, e, false)
			if mode == "already-covered" || mode == "ambiguous" {
				if ds := experienceDiscoveries(t, e); len(ds) != 0 || len(g.seen) != 0 {
					t.Fatal("drafting bypassed target classification", ds)
				}
				return
			}
			if mode == "enqueue-fault" {
				_, err := s.Store.db.Exec(`CREATE TRIGGER review_draft_fault BEFORE INSERT ON df_skill_generation_inputs BEGIN SELECT RAISE(ABORT,'fixture'); END`)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = e.RunDiscovery(ctx); err == nil {
					t.Fatal("generation insertion fault was not returned")
				}
				var jobs, consumed, daily int
				_ = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_generation_inputs`).Scan(&jobs)
				_ = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumed)
				_ = s.Store.db.QueryRow(`SELECT COALESCE(sum(reservations),0) FROM df_skill_discovery_days`).Scan(&daily)
				if jobs != 0 || consumed != 0 || daily != 0 {
					t.Fatal("partial draft admission committed", jobs, consumed, daily)
				}
				_, _ = s.Store.db.Exec(`DROP TRIGGER review_draft_fault`)
			}
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 || !ds[0].ReviewDraft {
				t.Fatal(ds)
			}
			if mode == "extra-file" {
				g.extra = true
			}
			if mode == "withdrawal" {
				control, _ := s.Store.Learning.Control(ctx, x.ID)
				if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
					t.Fatal(err)
				}
				if _, err := e.RunGeneration(ctx); err != nil {
					t.Fatal(err)
				}
				if len(g.seen) != 0 {
					t.Fatal("withdrawn source made generation call")
				}
			} else {
				if _, err := e.RunGeneration(ctx); err != nil {
					t.Fatal(err)
				}
				job, err := e.GetGeneration(ctx, ds[0].GenerationID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "extra-file" {
					if job.State != "failed" || job.ProposalID != "" {
						t.Fatal("draft added executable support file", job)
					}
				} else if mode == "receipt" {
					p, err := e.Get(ctx, job.ProposalID)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1`)
					if _, err = e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err == nil {
						t.Fatal("changed receipt authorized approval")
					}
				}
			}
			var consumed int
			_ = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumed)
			if consumed != 1 {
				t.Fatal("failed or withdrawn draft refunded its evidence", consumed)
			}
		})
	}
}

func TestSkillLearningReviewDraftNewCapabilityAutomaticallyDraftsOnce(t *testing.T) {
	s, x, _, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "complete-json-audit"
	consolidateExtract(t, s, x, "Perform a reusable local inspection")
	consolidateRun(t, s)
	experienceMatch(t, s)
	enableExperienceReviewDrafts(t, e, true)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 || !ds[0].ReviewDraft || ds[0].Base != (harness.SkillHead{}) || len(ds[0].Evidence) != 1 {
		t.Fatal(ds)
	}
	p := experienceGenerate(t, e)
	if p.Action != "create" || p.Status != "pending_review" || len(p.ChangedFiles) != 1 || len(g.seen) != 1 {
		t.Fatal(p, g.seen)
	}
	if _, err := e.Reject(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "Correct a detail in the same inspection")
	consolidateRun(t, s)
	experienceMatch(t, s)
	if got := experienceDiscoveries(t, e); len(got) != 1 || len(g.seen) != 1 {
		t.Fatal("revision or rejected output bypassed lifetime creation reservation", got)
	}
}
