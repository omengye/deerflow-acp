package runtime

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type consolidationTestExtractor struct{ observed bool }

func (e *consolidationTestExtractor) Identity() string { return "consolidation-extract-fixture-v1" }
func (e *consolidationTestExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	if err := consolidationTestCall(ctx, "experience_extract"); err != nil {
		return nil, err
	}
	refs := []string{}
	for _, ref := range input.References {
		if ref.Kind == "user" || e.observed && ref.Kind == "tool_observation" && ref.State == "completed" {
			refs = append(refs, ref.ID)
		}
	}
	c := harness.SkillExperienceCandidate{Kind: "correction_chain", Goal: "Inspect structured configuration", Applicability: "PowerShell JSON checks", Problem: "Incomplete line-by-line input", Resolution: "Read full file text", Steps: []string{"Read complete JSON text", "Validate required fields"}, Verification: "Receipt contains complete structured input", Outcome: "unresolved", References: refs}
	if e.observed {
		c.Outcome = "observed_execution"
	}
	return []harness.SkillExperienceCandidate{c}, nil
}

type consolidationTestModel struct {
	seen     []harness.SkillExperienceConsolidationInput
	relation string
	foreign  bool
	mutate   bool
	during   func()
}

func (e *consolidationTestModel) Identity() string { return "consolidation-fixture-v1" }
func (e *consolidationTestModel) Consolidate(ctx context.Context, input harness.SkillExperienceConsolidationInput) (harness.SkillExperienceConsolidationDecision, error) {
	e.seen = append(e.seen, input)
	if err := consolidationTestCall(ctx, "experience_consolidate"); err != nil {
		return harness.SkillExperienceConsolidationDecision{}, err
	}
	relation, target := "distinct", ""
	if len(input.Patterns) != 0 {
		relation, target = "equivalent", input.Patterns[0].ID
	}
	if e.relation != "" {
		relation = e.relation
		if relation == "distinct" || relation == "ambiguous" {
			target = ""
		}
	}
	if e.foreign {
		relation, target = "equivalent", "lpat_unauthorized"
		if e.mutate && len(input.Patterns) != 0 {
			input.Patterns[0].ID = target
		}
	}
	if e.during != nil {
		e.during()
	}
	return harness.SkillExperienceConsolidationDecision{Relation: relation, PatternID: target, Reason: "Compare the concrete input and environment token=PRIVATE_CONSOLIDATION_REASON"}, nil
}

func consolidationTestCall(ctx context.Context, stage string) error {
	b, ok := skillmaintenance.BrokerFromContext(ctx)
	if !ok {
		return harness.ErrSkillValidation
	}
	r, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: stage, ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 100})
	if err != nil {
		return err
	}
	return b.Settle(ctx, r, skillmaintenance.Usage{InputTokens: 10, OutputTokens: 5, Known: true})
}

func consolidationTestOptions() *harness.SkillLearningOptions {
	o := learningTestOptions()
	o.Budget.MaxDailyModelCalls, o.MaxDailyModelCalls = 30, 20
	o.Consolidation = &harness.SkillGenerationModelConfig{}
	return o
}

func newConsolidationTest(t *testing.T) (*Service, harness.Session, *consolidationTestExtractor, *consolidationTestModel) {
	t.Helper()
	s, x, _ := newLearningTest(t, true)
	_ = s.Store.Learning.Close()
	l, err := learning.New(context.Background(), s.Store.db, consolidationTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	extractor, model := &consolidationTestExtractor{observed: true}, &consolidationTestModel{}
	if err = l.Configure(context.Background(), extractor, nil); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureConsolidation(model); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	t.Cleanup(func() { _ = l.Close() })
	return s, x, extractor, model
}

func consolidateExtract(t *testing.T, s *Service, x harness.Session, text string) harness.RunRequest {
	t.Helper()
	req := learningTestRun(t, s, x, text)
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
	learningTestQueue(t, s)
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		t.Fatalf("extract=%v %v", worked, err)
	}
	return req
}

func consolidateQueued(t *testing.T, s *Service) {
	t.Helper()
	if worked, err := s.Store.Learning.QueueConsolidationOne(context.Background()); err != nil || !worked {
		// The persistent rotating scan wraps once, without model I/O.
		if err != nil {
			t.Fatal(err)
		}
		if worked, err = s.Store.Learning.QueueConsolidationOne(context.Background()); err != nil || !worked {
			t.Fatalf("consolidation queue=%v %v", worked, err)
		}
	}
}

func consolidateRun(t *testing.T, s *Service) {
	t.Helper()
	consolidateQueued(t, s)
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		t.Fatalf("consolidation=%v %v", worked, err)
	}
}

func TestSkillLearningConsolidationDistinctEpisodesAndUnresolvedEvidence(t *testing.T) {
	s, x, extractor, model := newConsolidationTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Check full configuration")
	consolidateRun(t, s)
	consolidateExtract(t, s, x, "Correct the previous check and validate fields")
	consolidateRun(t, s)
	page, err := s.SkillExperiencePatterns(ctx, "owner", x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].Revision != 2 || len(page.Patterns[0].ExperienceIDs) != 2 || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 || page.Patterns[0].EvidenceState != "insufficient_evidence" {
		t.Fatalf("one task inflated support %+v %v", page, err)
	}
	if page.Patterns[0].SourceState != "not_revalidated" {
		t.Fatal("history query falsely certified live sources", page.Patterns[0])
	}
	if _, err = s.SkillExperiencePatterns(ctx, "foreign-owner", x.ID, "", 10); err == nil {
		t.Fatal("foreign owner read patterns")
	}
	if _, err = s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "An independent JSON inspection")
	consolidateRun(t, s)
	page, err = s.SkillExperiencePatterns(ctx, "owner", x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].SupportEpisodes != 2 || page.Patterns[0].ObservedEpisodes != 2 || page.Patterns[0].EvidenceState != "observed_execution" || page.Patterns[0].Quality != "unassessed" {
		t.Fatalf("independent execution evidence lost %+v %v", page, err)
	}
	// Saved result reuse must also work for a merge whose input pinned the
	// previous revision, not only for an initially empty shortlist.
	var mergedJob string
	if err = s.Store.db.QueryRow(`SELECT job_id FROM df_learning_consolidations WHERE experience_id=?`, model.seen[2].Seed.ID).Scan(&mergedJob); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, mergedJob); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked || len(model.seen) != 3 {
		t.Fatal("merge result was not reused", worked, err)
	}
	extractor.observed = false
	consolidateExtract(t, s, x, "This step still has an unverified issue")
	consolidateRun(t, s)
	page, err = s.SkillExperiencePatterns(ctx, "owner", x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].EvidenceState != "unresolved" || page.Patterns[0].ObservedEpisodes != 2 || len(model.seen) != 4 {
		t.Fatalf("unresolved evidence disappeared %+v %v", page, err)
	}
	jobs, err := s.SkillLearningJobs(ctx, "owner", x.ID, "", 10)
	if err != nil || len(jobs.Jobs) != 8 {
		t.Fatal(jobs, err)
	}
	for _, job := range jobs.Jobs {
		if job.State != "completed" || job.Kind != "experience_extract" && job.Kind != "experience_consolidate" {
			t.Fatal("stage granted skill generation or failed", job)
		}
	}
	var reason []byte
	if err = s.Store.db.QueryRow(`SELECT result FROM df_learning_consolidations WHERE result_sha<>'' LIMIT 1`).Scan(&reason); err != nil || strings.Contains(string(reason), "PRIVATE_CONSOLIDATION_REASON") {
		t.Fatal("consolidation explanation retained secrets", err)
	}
}

func TestSkillLearningConsolidationConflictAmbiguityAndForeignTarget(t *testing.T) {
	for _, relation := range []string{"distinct", "conflicting", "ambiguous", "foreign", "mutated-shortlist"} {
		t.Run(relation, func(t *testing.T) {
			s, x, _, model := newConsolidationTest(t)
			consolidateExtract(t, s, x, "Initial procedure")
			consolidateRun(t, s)
			consolidateExtract(t, s, x, "Different or incompatible procedure")
			model.relation, model.foreign, model.mutate = relation, relation == "foreign" || relation == "mutated-shortlist", relation == "mutated-shortlist"
			consolidateRun(t, s)
			page, err := s.Store.Learning.Patterns(context.Background(), x.ID, "", 10)
			want := 2
			if model.foreign {
				want = 1
			}
			if err != nil || len(page.Patterns) != want {
				t.Fatal(page, err)
			}
			ambiguous := false
			for _, pattern := range page.Patterns {
				if pattern.SupportEpisodes != 1 {
					t.Fatal("model relation fabricated votes", pattern)
				}
				if relation == "conflicting" && (pattern.EvidenceState != "conflicting" || len(pattern.ConflictsWith) != 1) {
					t.Fatal("conflict not preserved symmetrically", pattern)
				}
				ambiguous = ambiguous || pattern.EvidenceState == "ambiguous"
			}
			if relation == "ambiguous" && !ambiguous {
				t.Fatal("ambiguity dropped")
			}
		})
	}
}

func TestSkillLearningConsolidationGuardRevocationChangedSourceAndRevision(t *testing.T) {
	for _, mode := range []string{"opt-out", "delete", "changed-event", "changed-revision", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, x, _, model := newConsolidationTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "Initial recipe")
			consolidateRun(t, s)
			req := consolidateExtract(t, s, x, "Correction to prior recipe")
			consolidateQueued(t, s)
			model.during = func() {
				switch mode {
				case "opt-out":
					_, err := s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1})
					if err != nil {
						t.Error(err)
					}
				case "delete":
					if err := executionTx(ctx, s.Store, func(tx *sql.Tx) error { return learning.ForgetSessionTx(ctx, tx, x.ID) }); err != nil {
						t.Error(err)
					}
				case "changed-event":
					_, err := s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: req.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "source changed"})
					if err != nil {
						t.Error(err)
					}
				case "changed-revision":
					if _, err := s.Store.db.Exec(`UPDATE df_learning_patterns SET current_revision=current_revision+1 WHERE session_id=?`, x.ID); err != nil {
						t.Error(err)
					}
				case "cancel":
					jobs, err := s.Store.Learning.Jobs(ctx, x.ID, "", 10)
					if err != nil {
						t.Error(err)
					}
					for _, job := range jobs.Jobs {
						if job.Kind == "experience_consolidate" && job.State == "running" {
							if err = s.Store.Learning.CancelExtraction(ctx, x.ID, job.ID, job.StateVersion); err != nil {
								t.Error(err)
							}
						}
					}
				}
			}
			if _, err := s.Store.Learning.RunOne(ctx); err != nil && !errors.Is(err, skillmaintenance.ErrConflict) {
				t.Fatal(err)
			}
			var count int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_pattern_revisions`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "opt-out" || mode == "delete" {
				want = 0
			}
			if count != want {
				t.Fatal("late derived text committed", count)
			}
			if mode == "opt-out" || mode == "delete" {
				if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_consolidations`).Scan(&count); err != nil || count != 0 {
					t.Fatal("forgotten consolidation text survived", count, err)
				}
			}
		})
	}
}

func TestSkillLearningConsolidationSavedResultRestartReuseAndCorruption(t *testing.T) {
	for _, mode := range []string{"valid", "corrupt-output", "missing-member"} {
		t.Run(mode, func(t *testing.T) {
			s, x, extractor, model := newConsolidationTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "First completed recipe")
			consolidateRun(t, s)
			// Simulate process death after the output transaction, before queue ACK.
			var job string
			if err := s.Store.db.QueryRow(`SELECT job_id FROM df_learning_consolidations LIMIT 1`).Scan(&job); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, job); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt-output" {
				if _, err := s.Store.db.Exec(`UPDATE df_learning_consolidations SET result='{}' WHERE job_id=?`, job); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-member" {
				if _, err := s.Store.db.Exec(`DELETE FROM df_learning_pattern_members`); err != nil {
					t.Fatal(err)
				}
			}
			_ = s.Store.Learning.Close()
			restarted, err := learning.New(ctx, s.Store.db, consolidationTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close() })
			if err = restarted.Configure(ctx, extractor, nil); err != nil {
				t.Fatal(err)
			}
			if err = restarted.ConfigureConsolidation(model); err != nil {
				t.Fatal(err)
			}
			if worked, err := restarted.RunOne(ctx); err != nil || !worked || len(model.seen) != 1 {
				t.Fatalf("saved result called model again %v %v calls=%d", worked, err, len(model.seen))
			}
			var state string
			if err = s.Store.db.QueryRow(`SELECT state FROM df_skill_maintenance_jobs WHERE id=?`, job).Scan(&state); err != nil || (state == "completed") != (mode == "valid") {
				t.Fatal("corrupt saved result acknowledged", state, err)
			}
		})
	}
}

func TestSkillLearningConsolidationModelReservationRechecksConsent(t *testing.T) {
	s, x, _, model := newConsolidationTest(t)
	consolidateExtract(t, s, x, "Completed procedure")
	consolidateQueued(t, s)
	// Disable the host before the claim/provider reservation; no adapter call.
	if _, err := s.Store.db.Exec(`UPDATE harness_learning_host SET enabled=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Learning.RunOne(context.Background()); err != nil || len(model.seen) != 0 {
		t.Fatal("revoked source reached model", err)
	}
}

func TestSkillLearningConsolidationInactiveHistoryAndSessionIsolation(t *testing.T) {
	s, x, extractor, model := newConsolidationTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Recipe in the first session")
	consolidateRun(t, s)
	other, err := s.NewSession(ctx, "owner", x.CWD)
	if err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, other, "Same wording in another session")
	consolidateRun(t, s)
	if len(model.seen) != 2 || len(model.seen[1].Patterns) != 0 {
		t.Fatal("consolidation exposed another session's text")
	}
	if err = s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	off, err := learning.New(ctx, s.Store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = off.Close() })
	page, err := off.Patterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].SourceState != "not_revalidated" {
		t.Fatal("disabled learning made paid history unreadable", page, err)
	}
	status, err := off.Status(ctx)
	if err != nil || status.Enabled || status.ConsolidationEnabled || status.Patterns != 2 {
		t.Fatal(status, err)
	}
	// Restore extraction-only mode. The absent consolidation gate cannot
	// resurrect a queue or claim a new model call.
	on, err := learning.New(ctx, s.Store.db, learningTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = on.Close() })
	if err = on.Configure(ctx, extractor, nil); err != nil {
		t.Fatal(err)
	}
	if worked, err := on.QueueConsolidationOne(ctx); err != nil || worked || len(model.seen) != 2 {
		t.Fatal("absent consolidation gate spent", err)
	}
}

func TestSkillLearningConsolidationResultAndMembersCommitAtomically(t *testing.T) {
	s, x, _, model := newConsolidationTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Recipe with an interrupted output commit")
	consolidateQueued(t, s)
	if _, err := s.Store.db.Exec(`CREATE TRIGGER fail_consolidation_members BEFORE INSERT ON df_learning_pattern_members
 BEGIN SELECT RAISE(ABORT,'fixture output failure'); END`); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked || len(model.seen) != 1 {
		t.Fatal(worked, err)
	}
	var patterns, outputs int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_patterns`).Scan(&patterns); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_consolidations WHERE result_sha<>''`).Scan(&outputs); err != nil || patterns != 0 || outputs != 0 {
		t.Fatal("partial consolidation output survived rollback", patterns, outputs, err)
	}
}

func TestSkillLearningConsolidationSharesExtractionSublimitWithoutRefund(t *testing.T) {
	s, x, extractor, model := newConsolidationTest(t)
	ctx := context.Background()
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	options := consolidationTestOptions()
	options.MaxDailyModelCalls = 1
	l, err := learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err = l.Configure(ctx, extractor, nil); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureConsolidation(model); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	consolidateExtract(t, s, x, "Procedure consumed the only learning call")
	consolidateQueued(t, s)
	for range 2 {
		if worked, err := l.RunOne(ctx); err != nil || worked {
			t.Fatal("quota-deferred consolidation was claimed", worked, err)
		}
	}
	var state, code string
	var attempts int
	var deadlineNS int64
	var charged int
	if err = s.Store.db.QueryRow(`SELECT state,error_code,attempts,deadline_ns FROM df_skill_maintenance_jobs WHERE job_kind='experience_consolidate'`).Scan(&state, &code, &attempts, &deadlineNS); err != nil || state != "queued" || code != "" || attempts != 0 || deadlineNS != 0 || len(model.seen) != 0 {
		t.Fatal("consolidation did not defer intact at the extraction sublimit", state, code, attempts, deadlineNS, len(model.seen), err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&charged); err != nil || charged != 1 {
		t.Fatal("denied call dispatched or previous receipt refunded", charged, err)
	}
	page, err := l.Patterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 0 {
		t.Fatal("budget denied result committed", page, err)
	}
}

func TestSkillLearningConsolidationPendingShortlistEpisodeDefersWithoutModel(t *testing.T) {
	s, x, _, model := newConsolidationTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Initial recipe")
	consolidateRun(t, s)
	if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "Independent pending consolidation")
	active := learningTestRun(t, s, x, "An earlier recipe's verification is still active")
	// A same-session resumed source can become active after its experience
	// was saved. It must delay a shortlist comparison rather than split it.
	var firstEpisode string
	if err := s.Store.db.QueryRow(`SELECT episode_id FROM df_learning_experiences ORDER BY created_at LIMIT 1`).Scan(&firstEpisode); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.db.Exec(`UPDATE harness_learning_inputs SET episode_id=? WHERE run_id=?`, firstEpisode, active.RunID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if worked, err := s.Store.Learning.QueueConsolidationOne(ctx); err != nil || worked || len(model.seen) != 1 {
			t.Fatal("active source compared or skipped", worked, err)
		}
	}
	var state, reason string
	if err := s.Store.db.QueryRow(`SELECT state,reason FROM df_learning_consolidation_inputs WHERE state<>'processed'`).Scan(&state, &reason); err != nil || state != "pending" || reason != "tree_unsettled" {
		t.Fatal("active shortlist did not remain pending", state, reason, err)
	}
}

func TestSkillLearningConsolidationMigrationDoesNotRecreateDeletedStageInputs(t *testing.T) {
	s, x, extractor, _ := newConsolidationTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Historical extraction before consolidation migration")
	if _, err := s.Store.db.Exec(`DELETE FROM df_learning_consolidation_inputs`); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := learning.New(ctx, s.Store.db, consolidationTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.Configure(ctx, extractor, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_consolidation_inputs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("startup resurrected missing stage inputs", count, err)
	}
	// Only the first schema migration may seed existing P1 experiences.
	if _, err = s.Store.db.Exec(`DELETE FROM df_learning_migrations WHERE name='consolidation-inputs-v1'`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = learning.Migrate(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_consolidation_inputs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("existing P1 experiences were not migrated", count, err)
	}
}
