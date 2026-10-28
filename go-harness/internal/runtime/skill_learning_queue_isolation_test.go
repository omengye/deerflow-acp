package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
)

func assertLearningSourceState(t *testing.T, s *Service, run, wantState, wantReason string) {
	t.Helper()
	var state, reason string
	if err := s.Store.db.QueryRow(`SELECT state,reason FROM harness_learning_outbox WHERE run_id=?`, run).Scan(&state, &reason); err != nil || state != wantState || reason != wantReason {
		t.Fatalf("source %s state=%s reason=%s, want %s/%s: %v", run, state, reason, wantState, wantReason, err)
	}
}

func TestSkillLearningQueueIsolatesOldUnfinishedRoots(t *testing.T) {
	for _, status := range []string{"waiting_input", "needs_reconciliation"} {
		t.Run(status, func(t *testing.T) {
			s, x, extractor := newLearningTest(t, true)
			old := learningTestRun(t, s, x, "OLD_UNFINISHED_INPUT")
			if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status=? WHERE id=?`, status, old.RunID); err != nil {
				t.Fatal(err)
			}
			first := learningTestRun(t, s, x, "Read JSON")
			learningTestFinish(t, s, first, "end_turn")
			second := learningTestRun(t, s, x, "Correction: read the complete file")
			learningTestFinish(t, s, second, "end_turn")
			learningTestQueue(t, s)
			if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
				var states string
				_ = s.Store.db.QueryRow(`SELECT group_concat(state||':'||reason) FROM harness_learning_outbox`).Scan(&states)
				t.Fatal("old unfinished root blocked extraction", worked, err, states)
			}
			if len(extractor.seen) != 1 {
				t.Fatal("safe turns were not extracted together", len(extractor.seen))
			}
			users := 0
			for _, ref := range extractor.seen[0].References {
				if ref.RunID == old.RunID || strings.Contains(ref.Text, "OLD_UNFINISHED_INPUT") {
					t.Fatal("unfinished source entered the snapshot", ref)
				}
				if ref.Kind == "user" {
					users++
				}
			}
			var unchanged string
			var episodes int
			if err := s.Store.db.QueryRow(`SELECT status FROM harness_runs WHERE id=?`, old.RunID).Scan(&unchanged); err != nil || unchanged != status || users != 2 {
				t.Fatal("isolation changed task state or lost correction context", unchanged, users, err)
			}
			if err := s.Store.db.QueryRow(`SELECT count(DISTINCT episode_id) FROM harness_learning_inputs`).Scan(&episodes); err != nil || episodes != 1 {
				t.Fatal("isolation fabricated independent episodes", episodes, err)
			}
			assertLearningSourceState(t, s, first.RunID, "processed", "")
			assertLearningSourceState(t, s, second.RunID, "processed", "")
		})
	}
}

func TestSkillLearningQueueUnsafeSourceDoesNotConsumeSafeTurn(t *testing.T) {
	for _, mode := range []string{"uncertain", "cancelled", "source_invalid"} {
		t.Run(mode, func(t *testing.T) {
			s, x, extractor := newLearningTest(t, true)
			unsafe := learningTestRun(t, s, x, "UNSAFE_SOURCE")
			if mode == "uncertain" {
				for _, kind := range []string{"tool_start", "tool_execute"} {
					event := receiptEvent(unsafe, kind, "uncertain-write", "write_file")
					if _, err := s.Store.Append(context.Background(), event); err != nil {
						t.Fatal(err)
					}
				}
			}
			reason := "end_turn"
			if mode == "cancelled" {
				reason = "cancelled"
			}
			learningTestFinish(t, s, unsafe, reason)
			if mode == "source_invalid" {
				if _, err := s.Store.db.Exec(`UPDATE harness_inputs SET content='[]' WHERE id=?`, unsafe.InputID); err != nil {
					t.Fatal(err)
				}
			}
			safe := learningTestRun(t, s, x, "Read complete configuration")
			learningTestFinish(t, s, safe, "end_turn")
			learningTestQueue(t, s)
			if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked || len(extractor.seen) != 1 {
				t.Fatal("mixed source batch lost safe input", worked, err, len(extractor.seen))
			}
			for _, ref := range extractor.seen[0].References {
				if ref.RunID != safe.RunID {
					t.Fatal("unsafe source entered safe extraction", ref)
				}
			}
			wantReason := "unsafe_outcome"
			if mode == "source_invalid" {
				wantReason = mode
			}
			assertLearningSourceState(t, s, unsafe.RunID, "skipped", wantReason)
			assertLearningSourceState(t, s, safe.RunID, "processed", "")
		})
	}
}

func TestSkillLearningQueuedJobResumesPastAbandonedContinuation(t *testing.T) {
	s, x, extractor := newLearningTest(t, true)
	first := learningTestRun(t, s, x, "Original finished check")
	learningTestFinish(t, s, first, "end_turn")
	learningTestQueue(t, s)
	abandoned := learningTestRun(t, s, x, "ABANDONED_CONTINUATION")
	if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status='needs_reconciliation' WHERE id=?`, abandoned.RunID); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || worked {
		t.Fatal("current unfinished continuation did not defer the queued job", worked, err)
	}
	later := learningTestRun(t, s, x, "Later safely finished check")
	learningTestFinish(t, s, later, "end_turn")
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		t.Fatal("queued job remained blocked after a later completed input", worked, err)
	}
	learningTestQueue(t, s)
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked || len(extractor.seen) != 2 {
		t.Fatal("later pending input could not follow the recovered queued job", worked, err)
	}
	for _, input := range extractor.seen {
		for _, ref := range input.References {
			if ref.RunID == abandoned.RunID {
				t.Fatal("recovery silently rebased onto unfinished evidence", ref)
			}
		}
	}
	users := 0
	for _, ref := range extractor.seen[1].References {
		if ref.Kind == "user" {
			users++
		}
	}
	if users != 2 || extractor.seen[0].EpisodeID != extractor.seen[1].EpisodeID {
		t.Fatal("recovery lost safe correction history or invented independence", users)
	}
	assertLearningSourceState(t, s, first.RunID, "processed", "")
	assertLearningSourceState(t, s, later.RunID, "processed", "")
}

func TestSkillLearningQueueKeepsOwnUnfinishedDescendantsPending(t *testing.T) {
	s, x, extractor := newLearningTest(t, true)
	parent := learningTestRun(t, s, x, "Wait for delegated verification")
	learningTestFinish(t, s, parent, "end_turn")
	now := timestamp()
	if _, err := s.Store.db.Exec(`INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES('isolated-child',?,'isolated-child-input','waiting_permission',?,?)`, x.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.BudgetLedger.BindMember(context.Background(), budget.MemberBinding{RootBudgetID: parent.RunID, MemberID: "isolated-child", SessionID: x.ID, OriginRunID: parent.RunID, ParentMemberID: parent.RunID, Kind: "task"}); err != nil {
		t.Fatal(err)
	}
	safe := learningTestRun(t, s, x, "Safely read another configuration")
	learningTestFinish(t, s, safe, "end_turn")
	learningTestQueue(t, s)
	assertLearningSourceState(t, s, parent.RunID, "pending", "tree_unsettled")
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked || len(extractor.seen) != 1 {
		t.Fatal("old child blocked unrelated safe root", worked, err)
	}
	for _, ref := range extractor.seen[0].References {
		if ref.RunID != safe.RunID {
			t.Fatal("unsettled root reached extractor", ref)
		}
	}
	if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status='completed',updated_at=? WHERE id='isolated-child'`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.Store.Learning.QueueOne(context.Background()); err != nil || !worked {
		t.Fatal("settled descendant did not automatically recover pending parent", worked, err)
	}
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked || len(extractor.seen) != 2 {
		t.Fatal(worked, err)
	}
	assertLearningSourceState(t, s, parent.RunID, "processed", "")
	if extractor.seen[0].EpisodeID != extractor.seen[1].EpisodeID {
		t.Fatal("recovery fabricated an independent episode")
	}
}

func TestSkillLearningQueueScansPastBlockedChunkAfterRestart(t *testing.T) {
	s, x, extractor := newLearningTest(t, true)
	for range 9 {
		req := learningTestRun(t, s, x, "Old blocked root")
		learningTestFinish(t, s, req, "end_turn")
		if _, err := s.Store.db.Exec(`UPDATE budget_roots SET blocked_reason='fixture' WHERE id=?`, req.RunID); err != nil {
			t.Fatal(err)
		}
	}
	safe := learningTestRun(t, s, x, "Safely finished historical pending input")
	learningTestFinish(t, s, safe, "end_turn")
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := learning.New(context.Background(), s.Store.db, learningTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if err = restarted.Configure(context.Background(), extractor, nil); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = restarted
	time.Sleep(1050 * time.Millisecond)
	if worked, err := restarted.QueueOne(context.Background()); err != nil || worked {
		t.Fatal("blocked first chunk was extracted", worked, err)
	}
	if worked, err := restarted.QueueOne(context.Background()); err != nil || !worked {
		t.Fatal("safe old pending input beyond first chunk starved", worked, err)
	}
	if worked, err := restarted.RunOne(context.Background()); err != nil || !worked || len(extractor.seen) != 1 {
		t.Fatal("restart did not automatically consume safe pending input", worked, err)
	}
	assertLearningSourceState(t, s, safe.RunID, "processed", "")
	var pending int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM harness_learning_outbox WHERE state='pending'`).Scan(&pending); err != nil || pending != 9 {
		t.Fatal("isolation swallowed pending blocked roots", pending, err)
	}
}

func TestSkillLearningQueueSplitsCombinedBoundsWithoutLosingSources(t *testing.T) {
	s, x, extractor := newLearningTest(t, true)
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	options := learningTestOptions()
	options.Model.MaxInputBytes = 8192
	l, err := learning.New(context.Background(), s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err = l.Configure(context.Background(), extractor, nil); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	var requests []harness.RunRequest
	for range 3 {
		req := learningTestRun(t, s, x, strings.Repeat("Structured configuration. ", 230))
		learningTestFinish(t, s, req, "end_turn")
		requests = append(requests, req)
	}
	time.Sleep(1050 * time.Millisecond)
	for range 3 {
		if worked, err := l.QueueOne(context.Background()); err != nil || !worked {
			t.Fatal("bounded source was lost while chunking", worked, err)
		}
		if worked, err := l.RunOne(context.Background()); err != nil || !worked {
			t.Fatal(worked, err)
		}
	}
	for _, req := range requests {
		assertLearningSourceState(t, s, req.RunID, "processed", "")
	}
	if len(extractor.seen) != 3 || extractor.seen[0].EpisodeID != extractor.seen[2].EpisodeID {
		t.Fatal("chunking lost sources or changed independence")
	}
}

func TestSkillLearningQueueIsolationConcurrentCollectorsDoNotDuplicate(t *testing.T) {
	s, x, extractor := newLearningTest(t, true)
	old := learningTestRun(t, s, x, "Old unfinished source")
	if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status='waiting_input' WHERE id=?`, old.RunID); err != nil {
		t.Fatal(err)
	}
	safe := learningTestRun(t, s, x, "Complete configuration check")
	learningTestFinish(t, s, safe, "end_turn")
	time.Sleep(1050 * time.Millisecond)
	runConcurrent := func(work func(context.Context) (bool, error)) {
		t.Helper()
		type result struct {
			worked bool
			err    error
		}
		results := make(chan result, 4)
		var workers sync.WaitGroup
		for range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				worked, err := work(context.Background())
				results <- result{worked, err}
			}()
		}
		workers.Wait()
		close(results)
		performed := 0
		for r := range results {
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.worked {
				performed++
			}
		}
		if performed != 1 {
			t.Fatal("concurrent collectors duplicated or lost isolated work", performed)
		}
	}
	runConcurrent(s.Store.Learning.QueueOne)
	runConcurrent(s.Store.Learning.RunOne)
	if len(extractor.seen) != 1 || learningCount(t, s.Store, "df_skill_maintenance_jobs") != 1 {
		t.Fatal("concurrent recovery duplicated immutable extraction")
	}
	assertLearningSourceState(t, s, safe.RunID, "processed", "")
}
