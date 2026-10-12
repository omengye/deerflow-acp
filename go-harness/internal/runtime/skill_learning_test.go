package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func learningTestOptions() *harness.SkillLearningOptions {
	return &harness.SkillLearningOptions{Idle: time.Second, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 10, MaxDailyTokens: 200000, MaxJobSeconds: 60, Workers: 1}, MaxDailyModelCalls: 3, MaxDailyTokens: 100000}
}

type learningTestExtractor struct {
	seen   []harness.SkillLearningInput
	during func()
	bad    bool
	empty  bool
}

func (e *learningTestExtractor) Identity() string { return "fixture-extraction-v1" }
func (e *learningTestExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	e.seen = append(e.seen, input)
	b, ok := skillmaintenance.BrokerFromContext(ctx)
	if !ok {
		return nil, harness.ErrSkillValidation
	}
	r, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: "experience_extract", ModelIdentity: strings.Repeat("a", 64), InputDigest: strings.Repeat("b", 64), InputTokens: 100, MaxOutputTokens: 100})
	if err != nil {
		return nil, err
	}
	if err = b.Settle(ctx, r, skillmaintenance.Usage{InputTokens: 10, OutputTokens: 5, Known: true}); err != nil {
		return nil, err
	}
	if e.during != nil {
		e.during()
	}
	if e.empty {
		return nil, nil
	}
	refs := []string{}
	for _, ref := range input.References {
		if ref.Kind == "user" {
			refs = append(refs, ref.ID)
		}
	}
	if e.bad {
		refs = append(refs, "input/foreign")
	}
	return []harness.SkillExperienceCandidate{{Kind: "correction_chain", Goal: "Read structured configuration", Applicability: "PowerShell JSON input", Problem: "Line-by-line parsing is incomplete", Resolution: "Use complete file text", Steps: []string{"Read the complete file"}, Verification: "Still requires independent verification", Outcome: "unresolved", References: refs}}, nil
}

func newLearningTest(t *testing.T, enabled bool) (*Service, harness.Session, *learningTestExtractor) {
	t.Helper()
	s := newSessionTestService(t)
	ledger, err := budget.New(s.Store.db, budget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.Store.BudgetLedger, s.Store.BudgetLimits = ledger, harness.DefaultBudgetLimits()
	var options *harness.SkillLearningOptions
	if enabled {
		options = learningTestOptions()
	}
	l, err := learning.New(context.Background(), s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	e := &learningTestExtractor{}
	if enabled {
		if err = l.Configure(context.Background(), e, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = l.Close() })
	x, err := s.NewSession(context.Background(), "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, x, e
}

func learningTestRun(t *testing.T, s *Service, x harness.Session, text string) harness.RunRequest {
	t.Helper()
	run := NewID()
	req := harness.RunRequest{Session: x, RunID: run, RootBudgetID: run, InputID: NewID(), Input: []harness.Content{{Type: "text", Text: text}}}
	if err := s.Store.BeginRun(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Append(context.Background(), harness.RunEvent{SessionID: x.ID, RunID: run, Kind: "reasoning_delta", Text: "PRIVATE_HIDDEN_REASONING"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Append(context.Background(), harness.RunEvent{SessionID: x.ID, RunID: run, Kind: "text_delta", Text: "Read the complete JSON file; verification pending."}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Append(context.Background(), harness.RunEvent{SessionID: x.ID, RunID: run, Kind: "text_delta", AgentName: "researcher", Text: "PRIVATE_CHILD_REPLY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Append(context.Background(), harness.RunEvent{SessionID: x.ID, RunID: run, Kind: "text_delta", AgentName: "deerflow", Text: "Read the complete JSON file; verification pending."}); err != nil {
		t.Fatal(err)
	}
	return req
}

func learningTestFinish(t *testing.T, s *Service, req harness.RunRequest, reason string) {
	t.Helper()
	if err := s.Store.Finish(context.Background(), req.RunID, reason, nil, foregroundBudgetScope(req)); err != nil {
		t.Fatal(err)
	}
}

func learningTestQueue(t *testing.T, s *Service) {
	t.Helper()
	// Wait for the configured idle window without modifying authoritative
	// terminal source receipts to make a test pass.
	time.Sleep(1050 * time.Millisecond)
	if worked, err := s.Store.Learning.QueueOne(context.Background()); err != nil || !worked {
		t.Fatalf("queue=%v %v", worked, err)
	}
}

func learningCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	if table != "harness_learning_inputs" && table != "harness_learning_outbox" && table != "df_learning_experiences" && table != "df_skill_maintenance_jobs" {
		t.Fatal("invalid fixture table")
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSkillLearningCaptureDefaultOffConsentAndEpisode(t *testing.T) {
	ctx := context.Background()
	off, x, _ := newLearningTest(t, false)
	q := learningTestRun(t, off, x, "No opt-in")
	learningTestFinish(t, off, q, "end_turn")
	if learningCount(t, off.Store, "harness_learning_inputs") != 0 || learningCount(t, off.Store, "harness_learning_outbox") != 0 {
		t.Fatal("default-off captured input")
	}
	s, x, _ := newLearningTest(t, true)
	first := learningTestRun(t, s, x, "Check JSON")
	learningTestFinish(t, s, first, "end_turn")
	second := learningTestRun(t, s, x, "No, read the complete file")
	learningTestFinish(t, s, second, "end_turn")
	var episodes int
	if err := s.Store.db.QueryRow(`SELECT count(DISTINCT episode_id) FROM harness_learning_inputs`).Scan(&episodes); err != nil || episodes != 1 {
		t.Fatalf("multiple roots counted independently: %d %v", episodes, err)
	}
	unchanged, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1, Contribute: true})
	if err != nil || unchanged.Epoch != 1 || learningCount(t, s.Store, "harness_learning_inputs") != 2 {
		t.Fatal("idempotent consent erased captured sources", unchanged, err)
	}
	if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	third := learningTestRun(t, s, x, "New unrelated task")
	learningTestFinish(t, s, third, "end_turn")
	if err := s.Store.db.QueryRow(`SELECT count(DISTINCT episode_id) FROM harness_learning_inputs`).Scan(&episodes); err != nil || episodes != 2 {
		t.Fatalf("explicit boundary missing: %d %v", episodes, err)
	}
	if _, err := s.SetSkillLearningControl(ctx, "foreign-owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1}); err == nil {
		t.Fatal("foreign owner changed consent")
	}
	control, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1})
	if err != nil || control.Epoch != 2 || control.Contribute {
		t.Fatalf("consent %+v %v", control, err)
	}
	unchanged, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 2})
	if err != nil || unchanged.Epoch != 2 {
		t.Fatal("idempotent opt-out changed epoch", unchanged, err)
	}
	if _, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1, Contribute: true}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("stale consent accepted: %v", err)
	}
	fourth := learningTestRun(t, s, x, "Opted-out turn")
	learningTestFinish(t, s, fourth, "end_turn")
	if learningCount(t, s.Store, "harness_learning_inputs") != 0 || learningCount(t, s.Store, "harness_learning_outbox") != 0 {
		t.Fatal("opt-out retained unpublished sources")
	}
}

func TestSkillLearningCaptureTerminalAtomicityAndEventSpoof(t *testing.T) {
	s, x, _ := newLearningTest(t, true)
	req := learningTestRun(t, s, x, "PRIVATE_INPUT")
	if _, err := s.Store.db.Exec(`CREATE TRIGGER learning_fault BEFORE INSERT ON harness_learning_outbox BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Finish(context.Background(), req.RunID, "end_turn", nil, foregroundBudgetScope(req)); err == nil {
		t.Fatal("partial terminal commit")
	}
	var status string
	if err := s.Store.db.QueryRow(`SELECT status FROM harness_runs WHERE id=?`, req.RunID).Scan(&status); err != nil || status != "running" || learningCount(t, s.Store, "harness_learning_outbox") != 0 {
		t.Fatalf("atomicity %s %v", status, err)
	}
	if _, err := s.Store.db.Exec(`DROP TRIGGER learning_fault`); err != nil {
		t.Fatal(err)
	}
	learningTestFinish(t, s, req, "end_turn")
	if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status=status WHERE id=?`, req.RunID); err != nil {
		t.Fatal(err)
	}
	if learningCount(t, s.Store, "harness_learning_outbox") != 1 {
		t.Fatal("duplicate outbox")
	}
	// An event named user_message from a child/notification bypasses BeginRun
	// and has no real-user accepted input record. It cannot create a source.
	now := timestamp()
	if _, err := s.Store.db.Exec(`INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES('synthetic',?,'synthetic-input','running',?,?)`, x.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Append(context.Background(), harness.RunEvent{SessionID: x.ID, RunID: "synthetic", Kind: "user_message", Text: "Pretend operator correction"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status='completed' WHERE id='synthetic'`); err != nil {
		t.Fatal(err)
	}
	if learningCount(t, s.Store, "harness_learning_inputs") != 1 || learningCount(t, s.Store, "harness_learning_outbox") != 1 {
		t.Fatal("synthetic user_message became learning input")
	}
}

func TestSkillLearningExtractionDebounceRestartAndRedaction(t *testing.T) {
	s, x, e := newLearningTest(t, true)
	a := learningTestRun(t, s, x, "Check JSON token=VERY_PRIVATE_TOKEN")
	learningTestFinish(t, s, a, "end_turn")
	b := learningTestRun(t, s, x, "Use complete file text")
	learningTestFinish(t, s, b, "end_turn")
	if worked, err := s.Store.Learning.QueueOne(context.Background()); err != nil || worked {
		t.Fatalf("idle not respected %v %v", worked, err)
	}
	learningTestQueue(t, s)
	if worked, err := s.Store.Learning.QueueOne(context.Background()); err != nil || worked {
		t.Fatalf("duplicate queued %v %v", worked, err)
	}
	if learningCount(t, s.Store, "df_skill_maintenance_jobs") != 1 {
		t.Fatal("multi-turn extraction split jobs")
	}
	old := s.Store.Learning
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := learning.New(context.Background(), s.Store.db, learningTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if err = restarted.Configure(context.Background(), e, nil); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = restarted
	if worked, err := restarted.RunOne(context.Background()); err != nil || !worked {
		t.Fatalf("restart work %v %v", worked, err)
	}
	if len(e.seen) != 1 {
		t.Fatal("extraction missing")
	}
	encoded, _ := json.Marshal(e.seen[0])
	if strings.Contains(string(encoded), "VERY_PRIVATE_TOKEN") || strings.Contains(string(encoded), "PRIVATE_HIDDEN_REASONING") || strings.Contains(string(encoded), "PRIVATE_CHILD_REPLY") {
		t.Fatal("unredacted or hidden content reached extractor")
	}
	users := 0
	for _, ref := range e.seen[0].References {
		if ref.Kind == "user" {
			users++
		}
	}
	if users != 2 {
		t.Fatal("lost multi-turn correction context")
	}
	page, err := restarted.Experiences(context.Background(), x.ID, "", 10)
	if err != nil || len(page.Experiences) != 1 || page.Experiences[0].Origin != "model_extracted" || page.Experiences[0].Quality != "unassessed" {
		t.Fatalf("experience %+v %v", page, err)
	}
	if worked, err := restarted.RunOne(context.Background()); err != nil || worked || len(e.seen) != 1 {
		t.Fatal("completed extraction replayed")
	}
}

func TestSkillLearningCaptureDeletionCancelsUnpublishedJobs(t *testing.T) {
	s, x, _ := newLearningTest(t, true)
	req := learningTestRun(t, s, x, "Read configuration")
	learningTestFinish(t, s, req, "end_turn")
	learningTestQueue(t, s)
	if err := s.Coordinator.Detach(context.Background(), x.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	alreadyDeleted, _, err := s.Store.purgeSession(context.Background(), x.ID, false)
	if err != nil || alreadyDeleted {
		t.Fatalf("alreadyDeleted=%v %v", alreadyDeleted, err)
	}
	if _, err := s.Store.Session(context.Background(), x.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("deleted session retained", err)
	}
	var state string
	if err = s.Store.db.QueryRow(`SELECT state FROM df_skill_maintenance_jobs`).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("job=%s %v", state, err)
	}
	if learningCount(t, s.Store, "harness_learning_inputs") != 0 || learningCount(t, s.Store, "harness_learning_outbox") != 0 || learningCount(t, s.Store, "df_learning_experiences") != 0 {
		t.Fatal("unpublished learning survived deletion")
	}
}

func TestSkillLearningExtractionChunkingPreservesSingleEpisode(t *testing.T) {
	s, x, e := newLearningTest(t, true)
	for i := 0; i < 10; i++ {
		req := learningTestRun(t, s, x, "A turn in one continuing task")
		learningTestFinish(t, s, req, "end_turn")
	}
	learningTestQueue(t, s)
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if worked, err := s.Store.Learning.QueueOne(context.Background()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if len(e.seen) != 2 || e.seen[0].EpisodeID != e.seen[1].EpisodeID {
		t.Fatal("chunking invented independent episodes")
	}
	var pending int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM harness_learning_outbox WHERE state IN ('pending','queued')`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal(pending, err)
	}
}

func TestSkillLearningExtractionCompletedEpisodeDoesNotWaitForNewTask(t *testing.T) {
	s, x, e := newLearningTest(t, true)
	old := learningTestRun(t, s, x, "Validate JSON")
	learningTestFinish(t, s, old, "end_turn")
	time.Sleep(1050 * time.Millisecond)
	if _, err := s.StartSkillLearningEpisode(context.Background(), "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	current := learningTestRun(t, s, x, "An unrelated active task")
	if worked, err := s.Store.Learning.QueueOne(context.Background()); err != nil || !worked {
		t.Fatal("completed task starved by unrelated episode", worked, err)
	}
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if len(e.seen) != 1 {
		t.Fatal("missing completed episode")
	}
	for _, ref := range e.seen[0].References {
		if ref.RunID != old.RunID {
			t.Fatal("unfinished task entered prior extraction", ref)
		}
	}
	learningTestFinish(t, s, current, "end_turn")
}

func TestSkillLearningExtractionSavedResultReusedAndVerified(t *testing.T) {
	for _, mode := range []string{"valid", "empty", "missing", "corrupted"} {
		t.Run(mode, func(t *testing.T) {
			s, x, e := newLearningTest(t, true)
			e.empty = mode == "empty"
			req := learningTestRun(t, s, x, "Inspect structured configuration")
			learningTestFinish(t, s, req, "end_turn")
			learningTestQueue(t, s)
			if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
				t.Fatal(worked, err)
			}
			// Simulate losing the terminal queue ACK after output committed.
			if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='retry',state_version=state_version+1,token='',lease_ns=0,retry_ns=0`); err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				if _, err := s.Store.db.Exec(`DELETE FROM df_learning_experiences`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupted" {
				if _, err := s.Store.db.Exec(`UPDATE df_learning_experiences SET payload='{}'`); err != nil {
					t.Fatal(err)
				}
			}
			if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked || len(e.seen) != 1 {
				t.Fatal("committed output triggered another model call", worked, err, len(e.seen))
			}
			var state, code string
			if err := s.Store.db.QueryRow(`SELECT state,error_code FROM df_skill_maintenance_jobs`).Scan(&state, &code); err != nil {
				t.Fatal(err)
			}
			if mode == "missing" || mode == "corrupted" {
				if state != "failed" || code != "invalid_candidate" {
					t.Fatal("invalid saved output acknowledged", state, code)
				}
			} else if state != "completed" || code != "" {
				t.Fatal(state, code)
			}
		})
	}
}

func TestSkillLearningExtractionJobListingAndCancellationCAS(t *testing.T) {
	s, x, e := newLearningTest(t, true)
	req := learningTestRun(t, s, x, "Inspect configuration")
	learningTestFinish(t, s, req, "end_turn")
	learningTestQueue(t, s)
	ctx := context.Background()
	page, err := s.SkillLearningJobs(ctx, "owner", x.ID, "", 10)
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].Kind != "experience_extract" || page.Jobs[0].State != "queued" {
		t.Fatal(page, err)
	}
	job := page.Jobs[0]
	if _, err = s.SkillLearningJobs(ctx, "foreign-owner", x.ID, "", 10); err == nil {
		t.Fatal("foreign owner saw job inventory")
	}
	if err = s.CancelSkillLearningExtraction(ctx, "owner", x.ID, job.ID, job.StateVersion+1); !errors.Is(err, skillmaintenance.ErrConflict) {
		t.Fatal("stale cancellation accepted", err)
	}
	if err = s.CancelSkillLearningExtraction(ctx, "owner", x.ID, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	page, err = s.SkillLearningJobs(ctx, "owner", x.ID, "", 10)
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].State != "cancelled" || page.Jobs[0].StateVersion != job.StateVersion+1 {
		t.Fatal(page, err)
	}
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || worked || len(e.seen) != 0 {
		t.Fatal("cancelled extraction dispatched", worked, err)
	}
}

func TestSkillLearningExtractionUnsafeAndUnsettledDoNotCall(t *testing.T) {
	for _, mode := range []string{"uncertain", "waiting-child", "blocked-budget", "paused", "unknown", "orphan-continued"} {
		t.Run(mode, func(t *testing.T) {
			s, x, e := newLearningTest(t, true)
			req := learningTestRun(t, s, x, "Read configuration")
			if mode == "uncertain" {
				for _, event := range []harness.RunEvent{{Kind: "tool_start", ToolCallID: "effect", ToolName: "write_file", Status: "pending", Arguments: []byte(`{"path":"fixture"}`)}, {Kind: "tool_execute", ToolCallID: "effect", ToolName: "write_file", Status: "in_progress"}} {
					event.SessionID, event.RunID = x.ID, req.RunID
					if _, err := s.Store.Append(context.Background(), event); err != nil {
						t.Fatal(err)
					}
				}
			}
			learningTestFinish(t, s, req, "end_turn")
			if mode == "waiting-child" {
				now := timestamp()
				if _, err := s.Store.db.Exec(`INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES('child',?,'child-input','waiting_permission',?,?)`, x.ID, now, now); err != nil {
					t.Fatal(err)
				}
				if err := s.Store.BudgetLedger.BindMember(context.Background(), budget.MemberBinding{RootBudgetID: req.RunID, MemberID: "child", SessionID: x.ID, OriginRunID: req.RunID, ParentMemberID: req.RunID, Kind: "task"}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "blocked-budget" {
				if _, err := s.Store.db.Exec(`UPDATE budget_roots SET blocked_reason='quota' WHERE id=?`, req.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "paused" || mode == "unknown" || mode == "orphan-continued" {
				state := mode
				if mode == "orphan-continued" {
					state = "continued"
				}
				if _, err := s.Store.db.Exec(`INSERT INTO budget_reservations(id,root_id,member_id,session_id,attempt_id,fence,kind,digest,input_tokens,output_cap,held_tokens,state) VALUES('fault',?,?,?,?,1,'tool','fixture',0,0,0,?)`, req.RunID, req.RunID, x.ID, req.RunID, state); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(1050 * time.Millisecond)
			if _, err := s.Store.Learning.QueueOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || worked || len(e.seen) != 0 {
				t.Fatalf("unsafe task extracted %v %v", worked, err)
			}
		})
	}
}

func TestSkillLearningExtractionRevocationRejectsLateAndForgedOutput(t *testing.T) {
	for _, action := range []string{"opt-out", "forget", "changed-event", "forged-ref", "cancelled"} {
		t.Run(action, func(t *testing.T) {
			s, x, e := newLearningTest(t, true)
			req := learningTestRun(t, s, x, "Read complete file")
			reason := "end_turn"
			if action == "cancelled" {
				reason = "cancelled"
			}
			learningTestFinish(t, s, req, reason)
			learningTestQueue(t, s)
			if action == "forged-ref" {
				e.bad = true
			}
			e.during = func() {
				switch action {
				case "opt-out":
					if _, err := s.Store.Learning.SetControl(context.Background(), x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1}); err != nil {
						t.Error(err)
					}
				case "forget":
					if err := executionTx(context.Background(), s.Store, func(tx *sql.Tx) error { return learning.ForgetSessionTx(context.Background(), tx, x.ID) }); err != nil {
						t.Error(err)
					}
				case "changed-event":
					if _, err := s.Store.Append(context.Background(), harness.RunEvent{SessionID: x.ID, RunID: req.RunID, Kind: "text_delta", Text: "changed after terminal"}); err != nil {
						t.Error(err)
					}
				}
			}
			worked, err := s.Store.Learning.RunOne(context.Background())
			if err != nil && !errors.Is(err, skillmaintenance.ErrConflict) {
				t.Fatal(err)
			}
			if action == "cancelled" && (worked || len(e.seen) != 0) {
				t.Fatal("cancelled outcome extracted")
			}
			if learningCount(t, s.Store, "df_learning_experiences") != 0 {
				t.Fatal("invalid output was promoted")
			}
		})
	}
}
