package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func uncertainWaitChild(t *testing.T, f *executionWaitFixture, state harness.ReceiptState) []byte {
	t.Helper()
	data := []byte(`{"state":"` + string(state) + `","error":"unconfirmed external effect"}`)
	if _, err := f.s.db.Exec(`INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,'unknown-effect',?,?,1,?)`, f.child.binding.TaskID, f.child.binding.ChildSessionID, state, data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFailedTerminalDependencyEndsAutomaticWaitWithoutReplay(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	f.complete(t)
	before := uncertainWaitChild(t, f, harness.ReceiptUncertain)
	engine := &automaticWaitEngine{}
	s := automaticService(t, f, engine)
	results := make(chan harness.RunResult, 1)
	subscribeAutomatic(t, s, "owner", f.req.Session.ID, harness.SessionUpdates{Result: func(_ context.Context, result harness.RunResult, err error) error {
		if !errors.Is(err, errFailedTaskWait) {
			t.Errorf("missing dependency failure: %v", err)
		}
		results <- result
		return nil
	}})
	select {
	case result := <-results:
		if result.Execution == nil || result.Execution.Status != harness.ExecutionFailed || result.Execution.Resumable {
			t.Fatalf("wait did not end: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal dependency left an endless wait")
	}
	if engine.resumes.Load() != 0 {
		t.Fatal("fault recovery replayed a checkpoint")
	}
	var after []byte
	if err := f.s.db.QueryRow(`SELECT receipt FROM harness_tool_receipts WHERE run_id=?`, f.child.binding.TaskID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("wait recovery rewrote child evidence", err)
	}
	if err := f.s.requireNoWaitingExecution(context.Background(), f.req.Session.ID); err != nil {
		t.Fatal("ended waiter still blocks new input", err)
	}
	state, err := f.s.Execution(context.Background(), f.req.Session.ID, f.req.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = executionTx(context.Background(), f.s, func(tx *sql.Tx) error {
		_, err := f.s.ClaimExecutionResumeTx(context.Background(), tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
		return err
	}); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("ended wait resumed old checkpoint", err)
	}
}

func TestLegacyFailedWaitOnlyClosesOnExplicitNewMessage(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	f.complete(t)
	uncertainWaitChild(t, f, harness.ReceiptUncertain)
	if _, err := f.s.db.Exec(`UPDATE harness_executions SET manifest=json_remove(manifest,'$.agentRecoveryVersion') WHERE run_id=?`, f.req.RunID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s := NewService(f.s, testEngine(func(_ context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls++
		if req.RunID == f.req.RunID || req.InputID == f.req.InputID {
			t.Error("fresh prompt replayed old execution")
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}), f.req.Session.Model)
	if _, err := s.Coordinator.Attach(f.req.Session.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if state, err := s.closeFailedTaskWait(ctx, "owner", f.req.Session.ID, ""); err != nil || state != nil {
		t.Fatal("automatic scan migrated legacy history", state, err)
	}
	if _, err := s.Run(ctx, "owner", f.req.Session.ID, []harness.Content{{Type: "text", Text: "Continue after that failure"}}, nil, nil); err != nil || calls != 1 {
		t.Fatal("explicit user message remains frozen", calls, err)
	}
	state, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil || state.Status != harness.ExecutionFailed {
		t.Fatal("old parent was not ended", state, err)
	}
}

func TestFailedWaitCleanupDoesNotEndLiveIO(t *testing.T) {
	for _, live := range []string{"started_receipt", "active_budget", "native_lease", "running_child"} {
		t.Run(live, func(t *testing.T) {
			f := newExecutionWaitFixture(t)
			f.suspend(t, false)
			if live != "running_child" {
				f.complete(t)
			}
			if live == "started_receipt" {
				uncertainWaitChild(t, f, harness.ReceiptStarted)
			} else {
				uncertainWaitChild(t, f, harness.ReceiptUncertain)
			}
			if live == "active_budget" {
				if _, err := f.s.db.Exec(`UPDATE budget_attempts SET state='active' WHERE member_id=?`, f.child.binding.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			if live == "native_lease" {
				if _, err := f.s.db.Exec(`UPDATE eino_background_tasks SET lease_expires_at=1 WHERE id=?`, f.child.binding.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			s := automaticService(t, f, &automaticWaitEngine{})
			if state, err := s.closeFailedTaskWait(context.Background(), "owner", f.req.Session.ID, ""); err != nil || state != nil {
				t.Fatal("active child was treated as ended", state, err)
			}
		})
	}
}

func TestFailedBackgroundWaitClosureIsAtomicAndLegacyWaitsRemainUnchanged(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_wait", true: "legacy_wait"}[legacy], func(t *testing.T) {
			f, child := pausedWaitFixture(t)
			ctx := context.Background()
			finishWaitTarget(t, f, child)
			if _, err := f.native.DB().Exec(`UPDATE harness_background_bindings SET blocked_reason='dependency cleanup failed' WHERE task_id=?`, child.Spec.ID); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if _, err := f.native.DB().Exec(`UPDATE harness_background_wait_manifests SET payload=json_remove(payload,'$.agentRecoveryVersion') WHERE task_id=?`, f.binding.TaskID); err != nil {
					t.Fatal(err)
				}
				if ready, err := f.permissions.ReadyWakeups(ctx); err != nil || len(ready) != 0 {
					t.Fatal("legacy wait changed", ready, err)
				}
				parent, err := f.native.Tasks().Get(ctx, f.binding.TaskID)
				if err != nil || parent.Status != bt.StatusWaitingInput {
					t.Fatal("automatic scan migrated legacy parent", parent, err)
				}
				return
			}
			if _, err := f.native.DB().Exec(`CREATE TRIGGER fail_wait_closure BEFORE UPDATE OF status ON harness_runs WHEN NEW.status='failed' BEGIN SELECT RAISE(ABORT,'injected wait closure failure'); END`); err != nil {
				t.Fatal(err)
			}
			if ready, err := f.permissions.ReadyWakeups(ctx); err == nil || len(ready) != 0 {
				t.Fatal("closure failure hidden", ready, err)
			}
			parent, err := f.native.Tasks().Get(ctx, f.binding.TaskID)
			if err != nil || parent.Status != bt.StatusWaitingInput || len(parent.Checkpoint) == 0 {
				t.Fatal("partial native failure escaped rollback", parent, err)
			}
			if _, err := f.native.DB().Exec(`DROP TRIGGER fail_wait_closure`); err != nil {
				t.Fatal(err)
			}
			if ready, err := f.permissions.ReadyWakeups(ctx); err != nil || len(ready) != 0 {
				t.Fatal("failed parent was resumed", ready, err)
			}
			parent, err = f.native.Tasks().Get(ctx, f.binding.TaskID)
			if err != nil || parent.Status != bt.StatusFailed || len(parent.Checkpoint) != 0 || parent.ResultError == "" {
				t.Fatal("parent did not end as failed", parent, err)
			}
			if _, err = f.native.Tasks().Resume(ctx, &bt.ResumeRequest{TaskID: parent.Spec.ID, ExpectedVersion: parent.Version, Data: []byte(`{}`)}); err == nil {
				t.Fatal("old failed parent resumed")
			}
		})
	}
}
