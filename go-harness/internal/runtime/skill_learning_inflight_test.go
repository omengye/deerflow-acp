package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func TestSkillLearningInFlightConversationKeepsOriginalBoundedJob(t *testing.T) {
	for _, stage := range []struct {
		kind          skillmaintenance.JobKind
		beforeReserve bool
	}{{skillmaintenance.ExperienceExtract, false}, {skillmaintenance.ExperienceConsolidate, false}, {skillmaintenance.SkillMatch, false}, {skillmaintenance.SkillMatch, true}} {
		name := string(stage.kind)
		if stage.beforeReserve {
			name += "-before-reserve"
		}
		t.Run(name, func(t *testing.T) {
			kind := stage.kind
			ctx := context.Background()
			var s *Service
			var x harness.Session
			var continuation harness.RunRequest
			var restart func()
			if kind == skillmaintenance.ExperienceExtract {
				var extractor *learningTestExtractor
				s, x, extractor = newLearningTest(t, true)
				extractor.during = func() {
					extractor.during = nil
					continuation = learningTestRun(t, s, x, "Also verify optional configuration fields")
				}
			} else if kind == skillmaintenance.ExperienceConsolidate {
				var model *consolidationTestModel
				s, x, _, model = newConsolidationTest(t)
				model.during = func() {
					model.during = nil
					continuation = learningTestRun(t, s, x, "Also verify optional configuration fields")
				}
			} else {
				var matcher *matchingTestModel
				var registry *skills.Registry
				var extractor *consolidationTestExtractor
				s, x, registry, extractor, matcher = newMatchingTest(t)
				action := func() {
					matcher.during, matcher.beforeReserve = nil, nil
					continuation = learningTestRun(t, s, x, "Also verify optional configuration fields")
				}
				if stage.beforeReserve {
					matcher.beforeReserve = action
				} else {
					matcher.during = action
				}
				restart = func() {
					learningInFlightRestart(t, s, registry, extractor, &consolidationTestModel{}, matcher)
				}
			}
			for _, text := range []string{"Inspect complete JSON configuration", "Correct the check with complete file text"} {
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
			if err != nil || before.State != "queued" {
				t.Fatal(before, err)
			}
			matchingRun(t, s)
			after, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || continuation.RunID == "" || after.State != "retry" || after.Attempts != 1 || after.ErrorCode != "source_active" || after.Request != before.Request || after.Deadline.IsZero() {
				t.Fatal("ordinary continuation permanently discarded in-flight learning", after, err)
			}
			assertCharges := func(want int) {
				t.Helper()
				var calls, tokens, unsettled int
				if err := s.Store.db.QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0),COALESCE(sum(state<>'settled'),0) FROM df_skill_maintenance_calls WHERE job_id=?`, id).Scan(&calls, &tokens, &unsettled); err != nil || calls != want || tokens != 15*want || unsettled != 0 {
					t.Fatal("in-flight work refunded or fabricated model charges", calls, tokens, unsettled, err)
				}
			}
			paid := 1
			if stage.beforeReserve {
				paid = 0
			}
			assertCharges(paid)
			if restart != nil {
				restart()
			}
			time.Sleep(time.Until(after.RetryAt) + 50*time.Millisecond)
			if worked, err := s.Store.Learning.RunOne(ctx); err != nil || worked {
				t.Fatal("busy retry spent another attempt", worked, err)
			}
			held, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || held != after {
				t.Fatal("waiting changed the original retry deadline or identity", held, after, err)
			}
			assertCharges(paid)
			learningQueueReadinessTurn(t, s, continuation)
			matchingRun(t, s)
			completed, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || completed.State != "completed" || completed.Attempts != 2 || completed.Request != before.Request || completed.Deadline != after.Deadline {
				t.Fatal("settled conversation did not automatically resume original work", completed, err)
			}
			assertCharges(paid + 1)
			var pending int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM harness_learning_outbox WHERE state='pending'`).Scan(&pending); err != nil || pending != 1 {
				t.Fatal("retry swallowed the later user turn", pending, err)
			}
			if kind == skillmaintenance.ExperienceExtract {
				page, err := s.Store.Learning.Experiences(ctx, x.ID, "", 10)
				if err != nil || len(page.Experiences) != 1 || len(page.Experiences[0].Candidate.References) != 2 {
					t.Fatal("retry silently rebased the captured two-turn experience", page, err)
				}
			} else {
				page, err := s.Store.Learning.Patterns(ctx, x.ID, "", 10)
				if err != nil || len(page.Patterns) != 1 || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 {
					t.Fatal("retry fabricated independent evidence", page, err)
				}
			}
		})
	}
}

func learningInFlightRestart(t *testing.T, s *Service, registry *skills.Registry, extractor harness.SkillExperienceExtractor, consolidator harness.SkillExperienceConsolidator, matcher harness.SkillExperienceMatcher) {
	t.Helper()
	ctx := context.Background()
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	options := consolidationTestOptions()
	options.Matching = &harness.SkillGenerationModelConfig{}
	next, err := learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	access, err := registry.NewMatchAccess(harness.SkillSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Configure(ctx, extractor, nil); err != nil {
		t.Fatal(err)
	}
	if err = next.ConfigureConsolidation(consolidator); err != nil {
		t.Fatal(err)
	}
	if err = next.ConfigureMatching(matcher, access); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = next
}

type learningInFlightEvaluator struct {
	*autoPatchLearningEvaluator
	after func()
}

func (e *learningInFlightEvaluator) Evaluate(ctx context.Context, in harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	result, err := e.autoPatchLearningEvaluator.Evaluate(ctx, in)
	if err == nil && e.calls == 2 && e.after != nil {
		e.after()
		e.after = nil
	}
	return result, err
}

func TestSkillLearningInFlightGenerationAndReviewResumeWithoutManualWork(t *testing.T) {
	for _, stage := range []string{"generation", "content", "behavior", "new-capability"} {
		t.Run(stage, func(t *testing.T) {
			s, x, registry, e, generator, validator, matcher := experienceDiscoveryTest(t)
			ctx := context.Background()
			newSkill := stage == "new-capability"
			if newSkill {
				matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
			} else {
				adoptLearningBase(t, registry)
				generator.preserveFrontmatter = true
			}
			evaluator := &learningInFlightEvaluator{autoPatchLearningEvaluator: &autoPatchLearningEvaluator{}}
			if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"Inspect complete JSON configuration", "Correct the check with complete file text"} {
				learningQueueReadinessTurn(t, s, learningTestRun(t, s, x, text))
			}
			learningTestQueue(t, s)
			matchingRun(t, s)
			consolidateRun(t, s)
			experienceMatch(t, s)
			enableExperienceReviewDrafts(t, e, newSkill)
			discoveries := experienceDiscoveries(t, e)
			if len(discoveries) != 1 || !discoveries[0].ReviewDraft {
				t.Fatal("ordinary conversation did not automatically queue a draft", discoveries)
			}
			id := discoveries[0].GenerationID
			before, err := e.GetGeneration(ctx, id)
			if err != nil || before.State != "queued" {
				t.Fatal(before, err)
			}
			var active harness.RunRequest
			begin := func() { active = learningTestRun(t, s, x, "Also check the optional fields") }
			switch stage {
			case "generation", "new-capability":
				generator.during = func() { generator.during = nil; begin() }
			case "content":
				validator.during = func() { validator.during = nil; begin() }
			case "behavior":
				evaluator.after = begin
			}
			if worked, err := e.RunGeneration(ctx); err != nil || !worked {
				t.Fatal("generation did not execute the original stage", worked, err)
			}
			retry, err := e.GetGeneration(ctx, id)
			if err != nil || active.RunID == "" || retry.State != "retry" || retry.Attempts != 1 || retry.ErrorCode != "source_active" || retry.Target != before.Target || retry.Base != before.Base {
				t.Fatal("ordinary continuation permanently lost automatic candidate work", retry, err)
			}
			generating := stage == "generation" || newSkill
			if generating && (retry.CandidateVersion != "" || retry.ProposalID != "") || !generating && (retry.CandidateVersion == "" || retry.ProposalID == "") {
				t.Fatal("active source committed a new candidate or lost its prior checkpoint", retry)
			}
			if retry.ProposalID != "" {
				p, err := e.Get(ctx, retry.ProposalID)
				if err != nil || p.Status != "validating" {
					t.Fatal("active review became publishable or terminal", p, err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			next, err := evolution.New(ctx, registry, validator)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = next.Close() })
			if err = next.ConfigureGeneration(ctx, generator, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			if err = next.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
				t.Fatal(err)
			}
			if err = next.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
				t.Fatal(err)
			}
			enableExperienceReviewDrafts(t, next, newSkill)
			time.Sleep(time.Until(retry.RetryAt) + 50*time.Millisecond)
			if worked, err := next.RunGeneration(ctx); err != nil || worked {
				t.Fatal("restart consumed a still-active retry", worked, err)
			}
			held, err := next.GetGeneration(ctx, id)
			if err != nil || held != retry {
				t.Fatal("restart changed the original candidate, attempt or deadline", held, retry, err)
			}
			learningQueueReadinessTurn(t, s, active)
			if worked, err := next.RunGeneration(ctx); err != nil || !worked {
				t.Fatal("settled source did not run the original generation", worked, err)
			}
			completed, err := next.GetGeneration(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := next.Get(ctx, completed.ProposalID)
			if err != nil || completed.State != "completed" || completed.Attempts != 2 || completed.Deadline != retry.Deadline || completed.Base != before.Base || proposal.Status != "pending_review" {
				t.Fatal("settled source did not resume its original candidate review", completed, proposal, err)
			}
			if !generating && (completed.CandidateVersion != retry.CandidateVersion || completed.ProposalID != retry.ProposalID) {
				t.Fatal("retry regenerated a saved candidate or proposal", completed, retry)
			}
			wantGeneration, wantContent, wantBehavior := 1, 1, 4
			wantBehaviorState := "passed"
			switch stage {
			case "generation":
				wantGeneration++
			case "new-capability":
				wantGeneration++
				wantBehavior, wantBehaviorState = 0, "not_run"
			case "content":
				wantContent++
			case "behavior":
				wantBehavior++
			}
			if len(generator.seen) != wantGeneration || validator.calls != wantContent || evaluator.calls != wantBehavior || proposal.Evaluation.Behavior != wantBehaviorState {
				t.Fatal("retry replayed durable content or completed behavior cases", len(generator.seen), validator.calls, evaluator.calls, proposal)
			}
			var calls, tokens, unsettled, consumed, feedback, pending int
			wantCalls := wantGeneration + 2*wantContent + wantBehavior
			if err := s.Store.db.QueryRow(`SELECT count(*),sum(charged_tokens),sum(state<>'settled') FROM df_skill_maintenance_calls WHERE job_id=?`, id).Scan(&calls, &tokens, &unsettled); err != nil || calls != wantCalls || tokens != 15*wantCalls || unsettled != 0 {
				t.Fatal("retry refunded paid calls or duplicated completed stages", calls, tokens, unsettled, err)
			}
			if err := s.Store.db.QueryRow(`SELECT (SELECT count(*) FROM df_skill_discovery_episode_consumed),(SELECT count(*) FROM df_skill_feedback),(SELECT count(*) FROM harness_learning_outbox WHERE state='pending')`).Scan(&consumed, &feedback, &pending); err != nil || consumed != 1 || feedback != 0 || pending != 1 {
				t.Fatal("retry duplicated consumption, quality votes or swallowed the later turn", consumed, feedback, pending, err)
			}
			if newSkill {
				var reservations int
				if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_new_skill_consumed`).Scan(&reservations); err != nil || reservations != 1 || proposal.Base != (harness.SkillHead{}) || proposal.SkillName != "structured-json-audit" {
					t.Fatal("automatic retry changed the reserved new capability", reservations, proposal, err)
				}
				if err := next.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
					t.Fatal(err)
				}
				if err := next.RunAutoPatch(ctx); err != nil {
					t.Fatal(err)
				}
				retained, err := next.Get(ctx, proposal.ID)
				if err != nil || retained.Status != "pending_review" {
					t.Fatal("retry granted automatic new-capability publication", retained, err)
				}
			}
			published, err := next.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
			if err != nil || published.Status != "published" {
				t.Fatal("system-generated candidate could not be reviewed after retry", published, err)
			}
		})
	}
}

func TestSkillLearningInFlightActivityCannotHidePermanentSourceChanges(t *testing.T) {
	for _, mode := range []string{"captured-event", "missing-outbox", "workspace", "catalog-head", "pattern-revision", "consent", "budget"} {
		t.Run(mode, func(t *testing.T) {
			s, x, registry, _, matcher := newMatchingTest(t)
			ctx := context.Background()
			captured := consolidateExtract(t, s, x, "Inspect the complete JSON configuration")
			consolidateRun(t, s)
			matchingQueue(t, s)
			id, _ := matchingJob(t, s)
			var active harness.RunRequest
			matcher.during = func() {
				active = learningTestRun(t, s, x, "Also verify optional fields")
				var err error
				switch mode {
				case "captured-event":
					_, err = s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: captured.RunID, Kind: "text_delta", Text: "Changed captured evidence"})
				case "missing-outbox":
					_, err = s.Store.db.Exec(`DELETE FROM harness_learning_outbox WHERE run_id=?`, captured.RunID)
				case "workspace":
					_, err = s.Store.db.Exec(`UPDATE harness_sessions SET cwd=? WHERE id=?`, t.TempDir(), x.ID)
				case "catalog-head":
					err = registry.SetEnabled(ctx, "project", "json-check", false)
				case "pattern-revision":
					_, err = s.Store.db.Exec(`UPDATE df_learning_patterns SET current_revision=current_revision+1 WHERE session_id=?`, x.ID)
				case "consent":
					control, readErr := s.Store.Learning.Control(ctx, x.ID)
					err = readErr
					if err == nil {
						_, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch})
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			worked, err := s.Store.Learning.RunOne(ctx)
			if !worked || err != nil && !(mode == "consent" && errors.Is(err, skillmaintenance.ErrConflict)) {
				t.Fatal(worked, err)
			}
			job, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "budget" {
				if job.State != "retry" || job.ErrorCode != "source_active" {
					t.Fatal("fixture did not enter active-source retry", job)
				}
				matcher.during = nil
				learningQueueReadinessTurn(t, s, active)
				// Exhaust the same durable global budget, not a replacement policy
				// or a refunded receipt, before the next paid admission.
				if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_days SET model_calls=30 WHERE day=?`, time.Now().UTC().Format("2006-01-02")); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Until(job.RetryAt) + 50*time.Millisecond)
				matchingRun(t, s)
				job, err = skillmaintenance.ReadJob(ctx, s.Store.db, id)
				if err != nil || job.State != "failed" || job.ErrorCode != "budget_exhausted" || job.Attempts != 2 {
					t.Fatal("active-source retry bypassed the paid budget", job, err)
				}
			} else if job.State != "failed" && job.State != "cancelled" || job.ErrorCode == "source_active" {
				t.Fatal("temporary activity hid permanent source or authority damage", job)
			}
			var saved, calls int
			if err := s.Store.db.QueryRow(`SELECT (SELECT count(*) FROM df_learning_matches WHERE job_id=? AND result_sha<>''),(SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?)`, id, id).Scan(&saved, &calls); err != nil || saved != 0 || calls != 1 {
				t.Fatal("invalid evidence was committed or refunded", saved, calls, err)
			}
		})
	}
}

func TestSkillLearningInFlightSharedActivityValidatesAllPinnedMembers(t *testing.T) {
	for _, mode := range []string{"continue", "other-source-damaged", "sharing-withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			s, first, model, matcher := newWorkspaceLearningTest(t, true)
			ctx := context.Background()
			workspaceLearningConsent(t, s, first, true)
			consolidateExtract(t, s, first, "First independent JSON check")
			consolidateRun(t, s)
			second := workspaceLearningSession(t, s, first.CWD)
			workspaceLearningConsent(t, s, second, true)
			captured := consolidateExtract(t, s, second, "Second independent JSON check")
			consolidateRun(t, s)
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
			var id string
			if err := s.Store.db.QueryRow(`SELECT id FROM df_skill_maintenance_jobs WHERE job_kind='skill_match' AND state='queued' ORDER BY rowid DESC LIMIT 1`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			var active harness.RunRequest
			matcher.during = func() {
				matcher.during = nil
				active = learningTestRun(t, s, first, "Continue verification of the first task")
				if mode == "other-source-damaged" {
					if _, err := s.Store.Append(ctx, harness.RunEvent{SessionID: second.ID, RunID: captured.RunID, Kind: "text_delta", Text: "Changed other pinned source"}); err != nil {
						t.Fatal(err)
					}
				} else if mode == "sharing-withdrawn" {
					workspaceLearningConsent(t, s, second, false)
				}
			}
			worked, err := s.Store.Learning.RunOne(ctx)
			if !worked || err != nil && !(mode == "sharing-withdrawn" && errors.Is(err, skillmaintenance.ErrConflict)) {
				t.Fatal(worked, err)
			}
			job, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "continue" {
				if job.State != "failed" && job.State != "cancelled" || job.ErrorCode == "source_active" {
					t.Fatal("active member hid damage or withdrawn sharing on another member", job)
				}
				return
			}
			if job.State != "retry" || job.ErrorCode != "source_active" {
				t.Fatal("active recalled member permanently discarded matching", job)
			}
			learningQueueReadinessTurn(t, s, active)
			time.Sleep(time.Until(job.RetryAt) + 50*time.Millisecond)
			matchingRun(t, s)
			completed, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || completed.State != "completed" || completed.Request != job.Request || completed.Deadline != job.Deadline || completed.Attempts != 2 || len(matcher.seen) != 2 || len(model.seen) != 2 {
				t.Fatal("shared retry changed its pinned source or repeated consolidation", completed, err)
			}
			page, err := s.Store.Learning.WorkspacePatterns(ctx, second.ID, "", 10)
			if err != nil || len(page.Patterns) != 1 || page.Patterns[0].SupportEpisodes != 2 || page.Patterns[0].ObservedEpisodes != 2 {
				t.Fatal("shared retry fabricated another independent vote", page, err)
			}
		})
	}
}
