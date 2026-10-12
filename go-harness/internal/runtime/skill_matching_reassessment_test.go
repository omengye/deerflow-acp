package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func matchingReassessmentChange(t *testing.T, s *Service, r *skills.Registry) {
	t.Helper()
	ctx := context.Background()
	if err := r.SetEnabled(ctx, "project", "json-check", false); err != nil {
		t.Fatal(err)
	}
	if err := r.SetEnabled(ctx, "project", "json-check", true); err != nil {
		t.Fatal(err)
	}
	// Simulate only the housekeeping cooldown, not captured evidence or pins.
	if _, err := s.Store.db.Exec(`UPDATE df_learning_matches SET created_at=?`, time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func matchingReassessmentPublishedChange(t *testing.T, s *Service, r *skills.Registry) {
	t.Helper()
	ctx := context.Background()
	var head harness.SkillHead
	var files map[string][]byte
	if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
		var err error
		head, err = tx.Current()
		if err != nil {
			return err
		}
		version, err := tx.ReadVersion(head.Version)
		if err == nil {
			files = version.Files()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	files["SKILL.md"] = append(files["SKILL.md"], []byte("Keep the documented environment boundary.\n")...)
	local, err := evolution.New(ctx, r, &experienceLocalValidator{})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	proposal, err := local.Submit(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "json-check", RequestKey: "reassessment-new-head", Action: "edit", Base: head, Reason: "Explicitly reviewed baseline change", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if proposal, err = local.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"}); err != nil || proposal.Status != "published" {
		t.Fatal(proposal, err)
	}
	if _, err := s.Store.db.Exec(`UPDATE df_learning_matches SET created_at=?`, time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLearningMatchingReassessmentAutomaticallyReachesReview(t *testing.T) {
	s, x, r, e, generator, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	for _, text := range []string{"Inspect complete JSON configuration", "Correct the check using full text"} {
		learningQueueReadinessTurn(t, s, learningTestRun(t, s, x, text))
	}
	learningTestQueue(t, s)
	matchingRun(t, s)
	consolidateRun(t, s)
	oldID := experienceMatch(t, s)
	oldJobID, _ := matchingJob(t, s)
	var payload, result []byte
	if err := s.Store.db.QueryRow(`SELECT payload,result FROM df_learning_matches WHERE id=?`, oldID).Scan(&payload, &result); err != nil {
		t.Fatal(err)
	}
	before, err := skillmaintenance.ReadJob(ctx, s.Store.db, oldJobID)
	if err != nil || before.State != "completed" {
		t.Fatal(before, err)
	}
	matchingReassessmentChange(t, s, r)
	matchingQueue(t, s)
	matchingRun(t, s)
	freshID, state := matchingJob(t, s)
	if freshID == oldJobID || state != "completed" || len(matcher.seen) != 2 {
		t.Fatal("changed inventory did not automatically create a fresh assessment", freshID, state, matcher.seen)
	}
	old, err := skillmaintenance.ReadJob(ctx, s.Store.db, oldJobID)
	if err != nil || old.Request != before.Request || old.State != before.State || old.Deadline != before.Deadline || old.Attempts != before.Attempts {
		t.Fatal("fresh assessment rewrote the old paid job", old, err)
	}
	var savedPayload, savedResult []byte
	if err := s.Store.db.QueryRow(`SELECT payload,result FROM df_learning_matches WHERE id=?`, oldID).Scan(&savedPayload, &savedResult); err != nil || string(savedPayload) != string(payload) || string(savedResult) != string(result) {
		t.Fatal("reassessment rewrote old fixed input or output", err)
	}
	page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
	if err != nil || len(page.Matches) != 2 {
		t.Fatal("historical paid result disappeared", page, err)
	}
	if _, err := s.Store.Learning.Promotion(ctx, oldID, 2, time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("old history granted live promotion authority")
	}
	enableExperienceReviewDrafts(t, e, false)
	discoveries := experienceDiscoveries(t, e)
	if len(discoveries) != 1 || !discoveries[0].ReviewDraft || len(discoveries[0].Evidence) != 1 {
		t.Fatal("automatic reassessment did not produce one review draft", discoveries)
	}
	proposal := experienceGenerate(t, e)
	if proposal.Status != "pending_review" || len(generator.seen) != 1 {
		t.Fatal(proposal, generator.seen)
	}
	approved, err := e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
	if err != nil || approved.Status != "published" {
		t.Fatal("human only approves the automatically generated candidate", approved, err)
	}
	var feedback, consumed, calls, tokens int
	if err := s.Store.db.QueryRow(`SELECT (SELECT count(*) FROM df_skill_feedback),(SELECT count(*) FROM df_skill_discovery_episode_consumed),count(*),sum(charged_tokens) FROM df_skill_maintenance_calls`).Scan(&feedback, &consumed, &calls, &tokens); err != nil || feedback != 0 || consumed != 1 || calls != 7 || tokens != 105 {
		t.Fatal("reassessment fabricated votes, refunded calls or duplicated generation", feedback, consumed, calls, tokens, err)
	}
}

func TestSkillLearningMatchingReassessmentFailedInputRestartAndFreshDecision(t *testing.T) {
	for _, point := range []string{"before-claim", "before-reserve", "after-paid", "saved-before-ack"} {
		t.Run(point, func(t *testing.T) {
			s, x, r, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "Inspect complete JSON input")
			consolidateRun(t, s)
			matchingQueue(t, s)
			oldID, _ := matchingJob(t, s)
			action := func() {
				matcher.beforeReserve, matcher.during = nil, nil
				matchingReassessmentChange(t, s, r)
			}
			switch point {
			case "before-claim":
				action()
			case "before-reserve":
				matcher.beforeReserve = action
			case "after-paid":
				matcher.during = action
			case "saved-before-ack":
				if _, err := s.Store.db.Exec(`CREATE TRIGGER reassessment_ack_fail BEFORE UPDATE OF state ON df_skill_maintenance_jobs
 WHEN OLD.id='` + oldID + `' AND NEW.state='completed' BEGIN SELECT RAISE(ABORT,'fixture ack'); END`); err != nil {
					t.Fatal(err)
				}
				if worked, err := s.Store.Learning.RunOne(ctx); err == nil || !worked {
					t.Fatal("fixture did not interrupt the saved matching ACK", worked, err)
				}
				if _, err := s.Store.db.Exec(`DROP TRIGGER reassessment_ack_fail; UPDATE df_skill_maintenance_jobs SET lease_ns=0 WHERE id=?`, oldID); err != nil {
					t.Fatal(err)
				}
				action()
			}
			matchingRun(t, s)
			old, err := skillmaintenance.ReadJob(ctx, s.Store.db, oldID)
			attempts := 1 + boolInt(point == "saved-before-ack")
			if err != nil || old.State != "failed" || old.ErrorCode != "inventory_changed" || old.Attempts != attempts {
				t.Fatal("inventory churn was retried or misclassified as source/configuration damage", old, err)
			}
			var charged int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?`, oldID).Scan(&charged); err != nil || charged != boolInt(point == "after-paid" || point == "saved-before-ack") {
				t.Fatal(charged, err)
			}
			// A fresh model decision, not the old unpaid/paid decision, controls
			// whether the changed skill still needs a proposal.
			matcher.relation = "already_covered"
			learningInFlightRestart(t, s, r, extractor, &consolidationTestModel{}, matcher)
			matchingQueue(t, s)
			freshID, _ := matchingJob(t, s)
			if freshID == oldID {
				t.Fatal("restart silently rebased the old job")
			}
			learningInFlightRestart(t, s, r, extractor, &consolidationTestModel{}, matcher)
			matchingRun(t, s)
			page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
			if err != nil || len(page.Matches) != 1+boolInt(point == "saved-before-ack") {
				t.Fatal("fresh assessment reused stale output", page, err)
			}
			for _, match := range page.Matches {
				if match.Relation == "already_covered" {
					continue
				}
				if point != "saved-before-ack" || match.Relation != "body_gap" {
					t.Fatal("unexpected historical or fresh result", match)
				}
			}
			ids, err := s.Store.Learning.PromotionIDsWithReviewDrafts(ctx, "", true, true)
			if err != nil || len(ids) != 0 {
				t.Fatal("covered procedure remained a candidate", ids, err)
			}
			oldAfter, err := skillmaintenance.ReadJob(ctx, s.Store.db, oldID)
			if err != nil || oldAfter.Request != old.Request || oldAfter.State != old.State || oldAfter.Deadline != old.Deadline || oldAfter.Attempts != old.Attempts {
				t.Fatal("old terminal work was resurrected", oldAfter, err)
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func matchingReassessmentScan(t *testing.T, s *Service) bool {
	t.Helper()
	worked := false
	for range 4 {
		one, err := s.Store.Learning.QueueMatchOne(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		worked = worked || one
	}
	return worked
}

func TestSkillLearningMatchingReassessmentBoundsCooldownAndQueueBudget(t *testing.T) {
	for _, mode := range []string{"unchanged", "cooldown", "window", "lifetime", "backpressure", "paid-budget", "atomic"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, _, matcher := newMatchingTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "Inspect complete JSON input")
			consolidateRun(t, s)
			matchingQueue(t, s)
			matchingRun(t, s)
			oldID, _ := matchingJob(t, s)
			if mode != "unchanged" {
				matchingReassessmentChange(t, s, r)
			}
			switch mode {
			case "unchanged", "cooldown", "window":
				if mode != "unchanged" {
					when := time.Now()
					if mode == "window" {
						when = when.Add(-31 * 24 * time.Hour)
					}
					if _, err := s.Store.db.Exec(`UPDATE df_learning_matches SET created_at=?`, when.UTC().Format(time.RFC3339Nano)); err != nil {
						t.Fatal(err)
					}
				}
				if matchingReassessmentScan(t, s) || len(matcher.seen) != 1 {
					t.Fatal("metadata scan generated work despite unchanged state or recovery bounds")
				}
				if mode == "cooldown" {
					matchingReassessmentChange(t, s, r)
					matchingQueue(t, s)
					matchingRun(t, s)
				}
			case "lifetime":
				for range 2 {
					matchingQueue(t, s)
					matchingRun(t, s)
					matchingReassessmentChange(t, s, r)
				}
				if matchingReassessmentScan(t, s) || len(matcher.seen) != 3 {
					t.Fatal("catalog churn created unbounded fresh jobs", matcher.seen)
				}
				var count int
				if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_match_reassessments`).Scan(&count); err != nil || count != 2 {
					t.Fatal(count, err)
				}
			case "backpressure":
				queue, err := skillmaintenance.New(ctx, s.Store.db, consolidationTestOptions().Budget)
				if err != nil {
					t.Fatal(err)
				}
				var backlog []skillmaintenance.Job
				for i := range 20 {
					job, err := queue.Enqueue(ctx, skillmaintenance.Request{Kind: skillmaintenance.SkillMatch, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("f", 64), Key: fmt.Sprint("backlog-", i), Reference: "fixture", InputDigest: strings.Repeat("e", 64)})
					if err != nil {
						t.Fatal(err)
					}
					backlog = append(backlog, job)
				}
				if matchingReassessmentScan(t, s) {
					t.Fatal("backpressure committed a partial fresh input")
				}
				var bound string
				var count int
				if err := s.Store.db.QueryRow(`SELECT job_id,(SELECT count(*) FROM df_learning_match_reassessments) FROM df_learning_match_inputs`).Scan(&bound, &count); err != nil || bound != oldID || count != 0 {
					t.Fatal("backpressure spent a lifetime slot or rebound history", bound, count, err)
				}
				for _, job := range backlog {
					if err := queue.Cancel(ctx, job.ID, job.StateVersion); err != nil {
						t.Fatal(err)
					}
				}
				matchingQueue(t, s)
				matchingRun(t, s)
			case "paid-budget":
				matchingQueue(t, s)
				// Exhaust only the existing global daily budget. Reassessment
				// admission must not create a second budget or refund old calls.
				if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_days SET model_calls=30`); err != nil {
					t.Fatal(err)
				}
				matchingRun(t, s)
				freshID, _ := matchingJob(t, s)
				fresh, err := skillmaintenance.ReadJob(ctx, s.Store.db, freshID)
				if err != nil || fresh.State != "failed" || fresh.ErrorCode != "budget_exhausted" {
					t.Fatal("fresh assessment escaped the shared paid budget", fresh, err)
				}
				var calls int
				if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?`, freshID).Scan(&calls); err != nil || calls != 0 {
					t.Fatal(calls, err)
				}
				matchingReassessmentChange(t, s, r)
				if matchingReassessmentScan(t, s) {
					t.Fatal("budget failure was resurrected as another fresh assessment")
				}
			case "atomic":
				if _, err := s.Store.db.Exec(`CREATE TRIGGER reassessment_fail BEFORE INSERT ON df_learning_match_reassessments BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
				_, err := s.Store.Learning.QueueMatchOne(ctx)
				if err == nil {
					_, err = s.Store.Learning.QueueMatchOne(ctx)
				}
				if err == nil {
					t.Fatal("fixture did not fail fresh assessment insertion")
				}
				var bound string
				var matches, jobs int
				if err := s.Store.db.QueryRow(`SELECT job_id,(SELECT count(*) FROM df_learning_matches),(SELECT count(*) FROM df_skill_maintenance_jobs) FROM df_learning_match_inputs`).Scan(&bound, &matches, &jobs); err != nil || bound != oldID || matches != 1 || jobs != 3 {
					t.Fatal("failed reassessment committed partial producer/job binding", bound, matches, jobs, err)
				}
				if _, err := s.Store.db.Exec(`DROP TRIGGER reassessment_fail`); err != nil {
					t.Fatal(err)
				}
				matchingQueue(t, s)
				matchingRun(t, s)
			}
		})
	}
}

func TestSkillLearningMatchingReassessmentCannotReviveDeniedOrGeneratedWork(t *testing.T) {
	for _, mode := range []string{"source", "workspace", "consent", "cancel", "ignore-restored", "adjudication", "selection", "model", "input-hash", "rehashed-input", "rehashed-result", "missing-input", "new-lifetime", "reserved", "pending-review"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, e, generator, _, matcher := experienceDiscoveryTest(t)
			ctx := context.Background()
			captured := consolidateExtract(t, s, x, "Inspect complete JSON input")
			consolidateRun(t, s)
			if mode == "cancel" {
				matchingQueue(t, s)
				id, _ := matchingJob(t, s)
				job, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Store.Learning.CancelExtraction(ctx, x.ID, id, job.StateVersion); err != nil {
					t.Fatal(err)
				}
			} else {
				experienceMatch(t, s)
			}
			if mode == "reserved" || mode == "pending-review" {
				enableExperienceReviewDrafts(t, e, false)
				if got := experienceDiscoveries(t, e); len(got) != 1 {
					t.Fatal(got)
				}
				if mode == "pending-review" {
					experienceGenerate(t, e)
				}
			}
			matchingReassessmentChange(t, s, r)
			var err error
			switch mode {
			case "source":
				_, err = s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: captured.RunID, Kind: "text_delta", Text: "Changed captured source"})
			case "workspace":
				_, err = s.Store.db.Exec(`UPDATE harness_sessions SET cwd=? WHERE id=?`, t.TempDir(), x.ID)
			case "consent":
				_, err = s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1})
			case "ignore-restored":
				p := learningReviewPattern(t, s, x)
				request := harness.SkillLearningPatternIgnoreRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision, ExpectedStateVersion: 0, Scope: "session", RequestKey: "ignore", Ignored: true}
				_, err = s.Store.Learning.SetPatternIgnored(ctx, x.ID, request)
				if err == nil {
					request.Ignored, request.ExpectedStateVersion, request.RequestKey = false, 1, "restore"
					_, err = s.Store.Learning.SetPatternIgnored(ctx, x.ID, request)
				}
			case "adjudication":
				p := learningReviewPattern(t, s, x)
				_, err = s.Store.Learning.AdjudicatePattern(ctx, x.ID, learningAdjudicationRequest(t, r, p, "json-check", "host-target"))
			case "selection", "model":
				_ = s.Store.Learning.Close()
				o := consolidationTestOptions()
				o.Matching = &harness.SkillGenerationModelConfig{}
				l, buildErr := learning.New(ctx, s.Store.db, o)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				t.Cleanup(func() { _ = l.Close() })
				if err = l.Configure(ctx, &consolidationTestExtractor{observed: true}, nil); err == nil {
					err = l.ConfigureConsolidation(&consolidationTestModel{})
				}
				selection := harness.SkillSelection{}
				if mode == "selection" {
					selection.Names = []string{}
				}
				access, accessErr := r.NewMatchAccess(selection)
				if accessErr != nil {
					t.Fatal(accessErr)
				}
				var adapter harness.SkillExperienceMatcher = matcher
				if mode == "model" {
					adapter = &matchingChangedModel{matcher}
				}
				if err == nil {
					err = l.ConfigureMatching(adapter, access)
				}
				s.Store.Learning = l
			case "input-hash":
				_, err = s.Store.db.Exec(`UPDATE df_learning_matches SET payload=CAST('broken' AS BLOB)`)
			case "rehashed-input", "rehashed-result":
				field := "payload"
				if mode == "rehashed-result" {
					field = "result"
					// Keep a valid hash and result envelope, but change only the
					// saved projection so it no longer follows its decision.
				}
				var raw []byte
				var id, job string
				if err = s.Store.db.QueryRow(`SELECT id,job_id,`+field+` FROM df_learning_matches LIMIT 1`).Scan(&id, &job, &raw); err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err = json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				if mode == "rehashed-input" {
					value["inventory_sha256"] = strings.Repeat("f", 64)
				} else {
					value["match"].(map[string]any)["reason"] = "Forged projection"
				}
				raw, _ = json.Marshal(value)
				sha := fmt.Sprintf("%x", sha256.Sum256(raw))
				_, err = s.Store.db.Exec(`UPDATE df_learning_matches SET `+field+`=?,`+field+`_sha=? WHERE id=?`, raw, sha, id)
				if mode == "rehashed-input" && err == nil {
					_, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET input_digest=? WHERE id=?`, sha, job)
				}
			case "missing-input":
				_, err = s.Store.db.Exec(`DELETE FROM df_learning_match_inputs`)
			case "new-lifetime":
				p := learningReviewPattern(t, s, x)
				tx, beginErr := s.Store.db.BeginTx(ctx, nil)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				if err = s.Store.Learning.ReserveNewPromotionTx(ctx, tx, p.ID, "previous-creation"); err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if matchingReassessmentScan(t, s) {
				t.Fatal("denied, controlled or already generated work got a fresh assessment", mode)
			}
			var count int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_match_reassessments`).Scan(&count); err != nil || count != 0 {
				t.Fatal("denial spent or rebound a reassessment slot", count, err)
			}
			if len(generator.seen) != boolInt(mode == "pending-review") || len(matcher.seen) != boolInt(mode != "cancel") {
				t.Fatal("scan called models outside admission", len(generator.seen), len(matcher.seen))
			}
		})
	}
}

func TestSkillLearningMatchingReassessmentSharedSourcesAndAutomaticPublication(t *testing.T) {
	for _, mode := range []string{"automatic-publish", "damaged-member", "sharing-revoked-after-queue"} {
		t.Run(mode, func(t *testing.T) {
			s, first, r, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			adoptLearningBase(t, r)
			_ = s.Store.Learning.Close()
			o := consolidationTestOptions()
			o.CrossSession, o.Matching = true, &harness.SkillGenerationModelConfig{}
			l, err := learning.New(ctx, s.Store.db, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if err := l.Configure(ctx, extractor, nil); err != nil {
				t.Fatal(err)
			}
			if err := l.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
				t.Fatal(err)
			}
			access, err := r.NewMatchAccess(harness.SkillSelection{})
			if err != nil {
				t.Fatal(err)
			}
			if err := l.ConfigureMatching(matcher, access); err != nil {
				t.Fatal(err)
			}
			s.Store.Learning = l
			generator, validator := &experienceDiscoveryGenerator{preserveFrontmatter: true}, &experienceDiscoveryValidator{}
			e, err := evolution.New(ctx, r, validator)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			if err := e.ConfigureGeneration(ctx, generator, o.Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			if err := e.ConfigureExperienceDiscoverySource(l); err != nil {
				t.Fatal(err)
			}
			second := workspaceLearningSession(t, s, first.CWD)
			var captured harness.RunRequest
			for _, session := range []harness.Session{first, second} {
				workspaceLearningConsent(t, s, session, true)
				for _, text := range []string{"Inspect the complete configuration for " + session.ID, "Verify required fields from full text"} {
					captured = learningTestRun(t, s, session, text)
					learningQueueReadinessTurn(t, s, captured)
				}
				learningTestQueue(t, s)
				matchingRun(t, s)
				consolidateRun(t, s)
			}
			experienceMatch(t, s)
			if mode == "automatic-publish" {
				matchingReassessmentPublishedChange(t, s, r)
			} else {
				matchingReassessmentChange(t, s, r)
			}
			if mode == "damaged-member" {
				if _, err := s.Store.Append(ctx, harness.RunEvent{SessionID: second.ID, RunID: captured.RunID, Kind: "text_delta", Text: "Changed shared member"}); err != nil {
					t.Fatal(err)
				}
				if matchingReassessmentScan(t, s) {
					t.Fatal("fresh assessment restored a damaged shared source")
				}
				return
			}
			// Activity on an original member must defer the metadata-only scan;
			// the ordinary continuation remains in its own terminal outbox.
			active := learningTestRun(t, s, first, "Check another optional field")
			if matchingReassessmentScan(t, s) {
				t.Fatal("fresh assessment captured an active shared conversation")
			}
			learningQueueReadinessTurn(t, s, active)
			matchingQueue(t, s)
			id, _ := matchingJob(t, s)
			if mode == "sharing-revoked-after-queue" {
				workspaceLearningConsent(t, s, second, false)
				job, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
				if err != nil || job.State != "cancelled" {
					t.Fatal("fresh assessment survived transitive sharing revocation", job, err)
				}
				if matchingReassessmentScan(t, s) {
					t.Fatal("source revoke restored erased matching inputs")
				}
				var count, charges int
				if err := s.Store.db.QueryRow(`SELECT (SELECT count(*) FROM df_learning_match_reassessments),count(*) FROM df_skill_maintenance_calls`).Scan(&count, &charges); err != nil || count != 0 || charges != 5 {
					t.Fatal("revocation kept derived identity or refunded receipts", count, charges, err)
				}
				return
			}
			matchingRun(t, s)
			page, err := l.WorkspacePatterns(ctx, first.ID, "", 10)
			if err != nil || len(page.Patterns) != 1 || page.Patterns[0].SupportEpisodes != 2 || page.Patterns[0].ObservedEpisodes != 2 {
				t.Fatal("fresh assessment turned a continuation into another independent vote", page, err)
			}
			enableExperienceDiscovery(t, e)
			evaluator := &autoPatchLearningEvaluator{}
			if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
				t.Fatal(err)
			}
			if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
				t.Fatal(err)
			}
			discoveries := experienceDiscoveries(t, e)
			if len(discoveries) != 1 || discoveries[0].ReviewDraft || len(discoveries[0].Evidence) != 2 {
				t.Fatal("fresh assessment did not retain independently authorized automatic mode", discoveries)
			}
			proposal := autoPatchLearningProposal(t, e, discoveries[0])
			if err := e.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			published, err := e.Get(ctx, proposal.ID)
			if err != nil || published.Status != "published" || len(generator.seen) != 1 || evaluator.calls != 4 {
				t.Fatal("fresh matching did not progress to guarded automatic publication", published, err)
			}
			var consumed, feedback int
			if err := s.Store.db.QueryRow(`SELECT (SELECT count(*) FROM df_skill_discovery_episode_consumed),(SELECT count(*) FROM df_skill_feedback)`).Scan(&consumed, &feedback); err != nil || consumed != 2 || feedback != 0 {
				t.Fatal("automatic publication fabricated quality or independent votes", consumed, feedback, err)
			}
			if matchingReassessmentScan(t, s) {
				t.Fatal("publication of this pattern recursively generated another assessment")
			}
		})
	}
}

func TestSkillLearningMatchingNewPatternRevisionUsesOrdinaryAcceptedEvidence(t *testing.T) {
	s, first, r, extractor, matcher := newMatchingTest(t)
	ctx := context.Background()
	_ = s.Store.Learning.Close()
	o := consolidationTestOptions()
	o.CrossSession, o.Matching = true, &harness.SkillGenerationModelConfig{}
	l, err := learning.New(ctx, s.Store.db, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := l.Configure(ctx, extractor, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
		t.Fatal(err)
	}
	access, err := r.NewMatchAccess(harness.SkillSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.ConfigureMatching(matcher, access); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	second := workspaceLearningSession(t, s, first.CWD)
	for _, session := range []harness.Session{first, second} {
		workspaceLearningConsent(t, s, session, true)
		consolidateExtract(t, s, session, "Inspect structured configuration for "+session.ID)
		consolidateRun(t, s)
		matchingQueue(t, s)
		matchingRun(t, s)
	}
	page, err := l.WorkspaceMatches(ctx, first.ID, "", 10)
	if err != nil || len(page.Matches) != 2 || len(matcher.seen) != 2 || matcher.seen[1].Revision != 2 {
		t.Fatal("new accepted task required manual rematching or rebased the old input", page, err)
	}
	var reassessments int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_match_reassessments`).Scan(&reassessments); err != nil || reassessments != 0 {
		t.Fatal("ordinary new revision improperly spent inventory-recovery slots", reassessments, err)
	}
}
