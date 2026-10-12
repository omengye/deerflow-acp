package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func learningQueueReadinessTurn(t *testing.T, s *Service, req harness.RunRequest) {
	t.Helper()
	for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
		event := receiptEvent(req, kind, "complete-file", "read_file")
		if kind == "tool_start" {
			event.Arguments = []byte(`{"path":"configuration.json"}`)
		}
		if kind == "tool_end" {
			event.Content = []harness.Content{{Type: "text", Text: `{"verified":true}`}}
		}
		if _, err := s.Store.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	learningTestFinish(t, s, req, "end_turn")
}

func TestSkillLearningQueuedConversationDefersWithoutLosingWork(t *testing.T) {
	for _, kind := range []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract, skillmaintenance.ExperienceConsolidate, skillmaintenance.SkillMatch} {
		t.Run(string(kind), func(t *testing.T) {
			s, x, registry, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			for _, text := range []string{"Inspect the configuration", "Use complete JSON text before validating fields"} {
				learningQueueReadinessTurn(t, s, learningTestRun(t, s, x, text))
			}
			learningTestQueue(t, s)
			if kind != skillmaintenance.ExperienceExtract {
				matchingRun(t, s)
				consolidateQueued(t, s)
			}
			if kind == skillmaintenance.SkillMatch {
				matchingRun(t, s)
				matchingQueue(t, s)
			}
			var id string
			if err := s.Store.db.QueryRow(`SELECT id FROM df_skill_maintenance_jobs WHERE job_kind=? ORDER BY rowid DESC LIMIT 1`, kind).Scan(&id); err != nil {
				t.Fatal(err)
			}
			before, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || before.State != "queued" || before.Attempts != 0 || !before.Deadline.IsZero() {
				t.Fatal("fixture did not queue a fresh stage", before, err)
			}
			continuation := learningTestRun(t, s, x, "Also verify the optional fields")
			for range 2 {
				if worked, err := s.Store.Learning.RunOne(ctx); err != nil || worked {
					t.Fatal("temporarily active task consumed the queued stage", worked, err)
				}
			}
			assertWaiting := func() {
				t.Helper()
				after, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
				if err != nil || after != before {
					t.Fatal("waiting changed the claim, attempt, deadline or source", before, after, err)
				}
				var calls int
				if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?`, id).Scan(&calls); err != nil || calls != 0 {
					t.Fatal("waiting spent model budget", calls, err)
				}
			}
			assertWaiting()
			if err = s.Store.Learning.Close(); err != nil {
				t.Fatal(err)
			}
			options := consolidationTestOptions()
			options.Matching = &harness.SkillGenerationModelConfig{}
			restarted, err := learning.New(ctx, s.Store.db, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close() })
			access, err := registry.NewMatchAccess(harness.SkillSelection{})
			if err != nil {
				t.Fatal(err)
			}
			if err = restarted.Configure(ctx, extractor, nil); err != nil {
				t.Fatal(err)
			}
			if err = restarted.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
				t.Fatal(err)
			}
			if err = restarted.ConfigureMatching(matcher, access); err != nil {
				t.Fatal(err)
			}
			s.Store.Learning = restarted
			if worked, err := restarted.RunOne(ctx); err != nil || worked {
				t.Fatal("restart consumed a still-active source", worked, err)
			}
			assertWaiting()
			learningQueueReadinessTurn(t, s, continuation)
			matchingRun(t, s)
			after, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || after.State != "completed" || after.Attempts != 1 || after.Request != before.Request {
				t.Fatal("settled source did not resume the original bounded job", after, err)
			}
			if worked, err := restarted.RunOne(ctx); err != nil || worked {
				t.Fatal("completed stage replayed model work", worked, err)
			}
			if kind == skillmaintenance.ExperienceExtract {
				page, err := restarted.Experiences(ctx, x.ID, "", 10)
				if err != nil || len(page.Experiences) != 1 || len(page.Experiences[0].Candidate.References) != 4 {
					t.Fatal("original two-turn source was lost or silently rebased", page, err)
				}
				var pending int
				if err = s.Store.db.QueryRow(`SELECT count(*) FROM harness_learning_outbox WHERE state='pending'`).Scan(&pending); err != nil || pending != 1 {
					t.Fatal("continuation was swallowed by the earlier snapshot", pending, err)
				}
			} else {
				page, err := restarted.Patterns(ctx, x.ID, "", 10)
				if err != nil || len(page.Patterns) != 1 || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 {
					t.Fatal("waiting fabricated independent experience", page, err)
				}
			}
			if kind == skillmaintenance.SkillMatch {
				page, err := restarted.Matches(ctx, x.ID, "", 10)
				if err != nil || len(page.Matches) != 1 || len(matcher.seen) != 1 {
					t.Fatal("original authorized matching stage did not finish", page, err)
				}
			}
		})
	}
}

func TestSkillLearningSharedQueuedConversationResumesToReview(t *testing.T) {
	for _, kind := range []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract, skillmaintenance.ExperienceConsolidate, skillmaintenance.SkillMatch, skillmaintenance.SkillGeneration} {
		t.Run(string(kind), func(t *testing.T) {
			s, x, registry, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			generator, validator := &experienceDiscoveryGenerator{}, &experienceDiscoveryValidator{}
			e, err := evolution.New(ctx, registry, validator)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			options := consolidationTestOptions()
			options.Matching = &harness.SkillGenerationModelConfig{}
			if err = e.ConfigureGeneration(ctx, generator, options.Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			if err = s.Store.Learning.Close(); err != nil {
				t.Fatal(err)
			}
			l, err := learning.New(ctx, s.Store.db, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			access, err := registry.NewMatchAccess(harness.SkillSelection{})
			if err != nil {
				t.Fatal(err)
			}
			if err = l.Configure(ctx, extractor, e.MaintenanceQueue()); err != nil {
				t.Fatal(err)
			}
			if err = l.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
				t.Fatal(err)
			}
			if err = l.ConfigureMatching(matcher, access); err != nil {
				t.Fatal(err)
			}
			if err = e.RegisterMaintenanceProducer(skillmaintenance.ExperienceExtract, l.ExtractionHooks(), l.Handle); err != nil {
				t.Fatal(err)
			}
			if err = e.RegisterMaintenanceProducer(skillmaintenance.ExperienceConsolidate, l.ConsolidationHooks(), l.HandleConsolidation); err != nil {
				t.Fatal(err)
			}
			if err = e.RegisterMaintenanceProducer(skillmaintenance.SkillMatch, l.MatchingHooks(), l.HandleMatch); err != nil {
				t.Fatal(err)
			}
			s.Store.Learning = l
			if err = e.ConfigureExperienceDiscoverySource(l); err != nil {
				t.Fatal(err)
			}
			enableExperienceReviewDrafts(t, e, false)
			runShared := func() {
				t.Helper()
				if worked, err := e.RunGeneration(ctx); err != nil || !worked {
					t.Fatal("shared dispatcher did not advance learning", worked, err)
				}
			}
			for _, text := range []string{"Inspect structured configuration", "Correct the check with the complete JSON document"} {
				learningQueueReadinessTurn(t, s, learningTestRun(t, s, x, text))
			}
			learningTestQueue(t, s)
			if kind != skillmaintenance.ExperienceExtract {
				runShared()
				consolidateQueued(t, s)
			}
			if kind == skillmaintenance.SkillMatch || kind == skillmaintenance.SkillGeneration {
				runShared()
				matchingQueue(t, s)
			}
			if kind == skillmaintenance.SkillGeneration {
				runShared()
				if discoveries := experienceDiscoveries(t, e); len(discoveries) != 1 || !discoveries[0].ReviewDraft {
					t.Fatal("fixture did not automatically queue generation", discoveries)
				}
			}
			var id string
			if err = s.Store.db.QueryRow(`SELECT id FROM df_skill_maintenance_jobs WHERE job_kind=? ORDER BY rowid DESC LIMIT 1`, kind).Scan(&id); err != nil {
				t.Fatal(err)
			}
			before, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil {
				t.Fatal(err)
			}
			continuation := learningTestRun(t, s, x, "Also check optional fields using the same method")
			if worked, err := e.RunGeneration(ctx); err != nil || worked {
				t.Fatal("shared dispatcher consumed temporarily active evidence", worked, err)
			}
			after, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || after != before {
				t.Fatal("shared waiting changed the original job", before, after, err)
			}
			learningQueueReadinessTurn(t, s, continuation)
			runShared()
			if kind == skillmaintenance.ExperienceExtract {
				consolidateQueued(t, s)
				runShared()
			}
			if kind == skillmaintenance.ExperienceExtract || kind == skillmaintenance.ExperienceConsolidate {
				matchingQueue(t, s)
				runShared()
			}
			discoveries := experienceDiscoveries(t, e)
			if len(discoveries) != 1 || !discoveries[0].ReviewDraft || len(discoveries[0].Evidence) != 1 {
				t.Fatal("resumed learning did not automatically discover one review-only candidate", discoveries)
			}
			var proposal harness.SkillProposal
			if kind == skillmaintenance.SkillGeneration {
				page, err := e.QueryProposals(ctx, harness.SkillQuery{Limit: 10})
				if err != nil || len(page.Proposals) != 1 {
					t.Fatal("resumed generation did not finish its original job", page, err)
				}
				proposal = page.Proposals[0]
			} else {
				proposal = experienceGenerate(t, e)
			}
			if proposal.Status != "pending_review" || len(generator.seen) != 1 || !generator.seen[0].InstructionOnly || validator.calls != 1 {
				t.Fatal("resumed source did not produce the automatic candidate", proposal, generator.seen, validator.calls)
			}
			published, err := e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
			if err != nil || published.Status != "published" {
				t.Fatal("resumed evidence failed normal human review", published, err)
			}
			var calls, tokens int
			if err = s.Store.db.QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0) FROM df_skill_maintenance_calls`).Scan(&calls, &tokens); err != nil || calls != 6 || tokens != 90 {
				t.Fatal("waiting repeated model work or reset shared accounting", calls, tokens, err)
			}
		})
	}
}

func TestSkillLearningQueuedSharedMembersWaitOnlyForCapturedEpisodes(t *testing.T) {
	for _, kind := range []skillmaintenance.JobKind{skillmaintenance.ExperienceConsolidate, skillmaintenance.SkillMatch} {
		t.Run(string(kind), func(t *testing.T) {
			s, first, model, matcher := newWorkspaceLearningTest(t, true)
			ctx := context.Background()
			workspaceLearningConsent(t, s, first, true)
			consolidateExtract(t, s, first, "First independently observed JSON check")
			consolidateRun(t, s)
			second := workspaceLearningSession(t, s, first.CWD)
			workspaceLearningConsent(t, s, second, true)
			consolidateExtract(t, s, second, "Second independently observed JSON check")
			consolidateQueued(t, s)
			if kind == skillmaintenance.SkillMatch {
				matchingRun(t, s)
				for range 8 {
					if _, err := s.Store.Learning.QueueMatchOne(ctx); err != nil {
						t.Fatal(err)
					}
					var queued bool
					if err := s.Store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM df_skill_maintenance_jobs WHERE job_kind='skill_match' AND state='queued')`).Scan(&queued); err != nil {
						t.Fatal(err)
					}
					if queued {
						break
					}
				}
			}
			var id string
			if err := s.Store.db.QueryRow(`SELECT id FROM df_skill_maintenance_jobs WHERE job_kind=? ORDER BY rowid DESC LIMIT 1`, kind).Scan(&id); err != nil {
				t.Fatal(err)
			}
			before, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil {
				t.Fatal(err)
			}
			active := learningTestRun(t, s, first, "Continue the first task verification")
			if worked, err := s.Store.Learning.RunOne(ctx); err != nil || worked {
				t.Fatal("active recalled member was skipped instead of deferred", worked, err)
			}
			after, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || after != before {
				t.Fatal("shared member activity consumed the pinned job", before, after, err)
			}
			learningQueueReadinessTurn(t, s, active)
			// Explicit boundaries here test host episode isolation, not a
			// prerequisite for ordinary review-only candidate generation.
			if _, err = s.StartSkillLearningEpisode(ctx, "owner", first.ID); err != nil {
				t.Fatal(err)
			}
			unrelated := learningTestRun(t, s, first, "A new unrelated task is active")
			matchingRun(t, s)
			after, err = skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || after.State != "completed" || after.Attempts != 1 || after.Request != before.Request {
				t.Fatal("later unrelated episode blocked captured shared evidence", after, err)
			}
			patterns, err := s.Store.Learning.WorkspacePatterns(ctx, second.ID, "", 10)
			if err != nil || len(patterns.Patterns) != 1 || patterns.Patterns[0].SupportEpisodes != 2 || patterns.Patterns[0].ObservedEpisodes != 2 || len(model.seen) != 2 {
				t.Fatal("shared waiting changed source attribution", patterns, len(model.seen), err)
			}
			if kind == skillmaintenance.SkillMatch {
				page, err := s.Store.Learning.WorkspaceMatches(ctx, second.ID, "", 10)
				if err != nil || len(page.Matches) != 1 || len(matcher.seen) != 1 {
					t.Fatal("shared members did not finish authorized matching", page, err)
				}
			}
			learningTestFinish(t, s, unrelated, "end_turn")
		})
	}
}

func TestSkillLearningQueuedReviewACKDoesNotWaitForLaterConversation(t *testing.T) {
	s, x, _, e, generator, validator, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Inspect the complete JSON document")
	consolidateRun(t, s)
	experienceMatch(t, s)
	enableExperienceReviewDrafts(t, e, false)
	if discoveries := experienceDiscoveries(t, e); len(discoveries) != 1 || !discoveries[0].ReviewDraft {
		t.Fatal("fixture did not queue the review-only candidate", discoveries)
	}
	proposal := experienceGenerate(t, e)
	var id string
	if err := s.Store.db.QueryRow(`SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, proposal.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Retain a committed review with its original attempt/deadline; only the
	// final maintenance ACK was lost, so no paid stage needs to resume.
	if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='retry',token='',lease_ns=0,retry_ns=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	active := learningTestRun(t, s, x, "Continue checking optional fields")
	if worked, err := e.RunGeneration(ctx); err != nil || !worked {
		t.Fatal("durable review ACK waited on a later task", worked, err)
	}
	job, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
	if err != nil || job.State != "completed" || len(generator.seen) != 1 || validator.calls != 1 {
		t.Fatal("durable review was replayed instead of acknowledged", job, len(generator.seen), validator.calls, err)
	}
	got, err := e.Get(ctx, proposal.ID)
	if err != nil || got.Status != "pending_review" || got.StateVersion != proposal.StateVersion {
		t.Fatal("ACK adopted or changed the human-review candidate", got, err)
	}
	if _, err = e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("ACK allowed publication while the source was active")
	}
	got, err = e.Get(ctx, proposal.ID)
	if err != nil || got.Status != "pending_review" || got.StateVersion != proposal.StateVersion {
		t.Fatal("temporary source activity irreversibly invalidated review", got, err)
	}
	learningQueueReadinessTurn(t, s, active)
	published, err := e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
	if err != nil || published.Status != "published" {
		t.Fatal("settled source could not use its original reviewed candidate", published, err)
	}
	var calls, tokens int
	if err = s.Store.db.QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0) FROM df_skill_maintenance_calls`).Scan(&calls, &tokens); err != nil || calls != 6 || tokens != 90 {
		t.Fatal("ACK repeated or refunded earlier paid work", calls, tokens, err)
	}
}

func TestSkillLearningAutoPatchWaitsForConversationThenPublishesOriginalCandidate(t *testing.T) {
	s, x, registry, e, generator, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	adoptLearningBase(t, registry)
	generator.preserveFrontmatter = true
	evaluator := &autoPatchLearningEvaluator{}
	if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
		t.Fatal(err)
	}
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	discoveries := experienceDiscoveries(t, e)
	if len(discoveries) != 1 || discoveries[0].ReviewDraft {
		t.Fatal("fixture did not supply independent automatic-publication evidence", discoveries)
	}
	proposal := autoPatchLearningProposal(t, e, discoveries[0])
	if proposal.Evaluation.Behavior != "passed" || evaluator.calls != 4 {
		t.Fatal("candidate did not improve the trusted comparison", proposal, evaluator.calls)
	}
	active := learningTestRun(t, s, x, "Continue the second task verification")
	if err := e.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	waiting, err := e.Get(ctx, proposal.ID)
	if err != nil || waiting.Status != "pending_review" || waiting.StateVersion != proposal.StateVersion {
		t.Fatal("temporary source activity permanently staled the automatic candidate", waiting, err)
	}
	learningQueueReadinessTurn(t, s, active)
	if err = e.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	published, err := e.Get(ctx, proposal.ID)
	if err != nil || published.Status != "published" || published.CandidateVersion != proposal.CandidateVersion || len(generator.seen) != 1 || evaluator.calls != 4 {
		t.Fatal("automatic publication required manual regeneration or repeated paid evaluation", published, err)
	}
	observations, err := e.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(observations.Observations) != 1 || observations.Observations[0].Successes != 0 || observations.Observations[0].Failures != 0 {
		t.Fatal("conversation waiting fabricated quality observations", observations, err)
	}
}
