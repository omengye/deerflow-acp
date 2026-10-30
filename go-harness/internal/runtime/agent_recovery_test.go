package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func TestAgentRecoveryReceiptPreservesUnknownEffectsAndCallIdentity(t *testing.T) {
	ctx := context.Background()
	s, req := receiptFixture(t)
	for _, kind := range []string{"tool_start", "tool_execute"} {
		e := receiptEvent(req, kind, "unknown-write", "execute")
		e.Receipt = &harness.ToolReceipt{AgentRecoverable: true}
		saved, err := s.Store.Append(ctx, e)
		if err != nil || saved.Receipt.AgentRecoverable {
			t.Fatalf("non-terminal event set recovery marker: %+v %v", saved, err)
		}
	}
	end := receiptEvent(req, "tool_end", "unknown-write", "execute")
	end.Status = "failed"
	end.Content = []harness.Content{{Type: "text", Text: "connection lost after dispatch; effects unconfirmed"}}
	end.Receipt = &harness.ToolReceipt{AgentRecoverable: true, State: harness.ReceiptUncertain, FailureCode: "uncertain"}
	saved, err := s.Store.Append(ctx, end)
	if err != nil || saved.Receipt.State != harness.ReceiptUncertain || !saved.Receipt.AgentRecoverable || saved.Receipt.Error == "" || len(saved.Receipt.Result) != 1 {
		t.Fatalf("failure evidence changed: %+v %v", saved, err)
	}
	stored, err := readReceipt(ctx, s.Store.db, req.Session.ID, req.RunID, end.ToolCallID)
	if err != nil || !stored.AgentRecoverable || stored.State != harness.ReceiptUncertain || stored.Review != nil {
		t.Fatalf("durable evidence changed: %+v %v", stored, err)
	}
	if err = s.Store.requireReconciled(ctx, req.Session.ID); err != nil {
		t.Fatal("agent recovery froze the session", err)
	}
	if err = executionTx(ctx, s.Store, func(tx *sql.Tx) error {
		_, err := executionReceiptFrontier(ctx, tx, req.Session.ID, req.RunID)
		return err
	}); err != nil {
		t.Fatal("terminal failure blocked a later checkpoint", err)
	}
	for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
		if _, err = s.Store.Append(ctx, receiptEvent(req, kind, end.ToolCallID, end.ToolName)); !errors.Is(err, harness.ErrReceiptConflict) {
			t.Fatalf("old call replay accepted for %s: %v", kind, err)
		}
	}
}

func TestAgentRecoveryAllowsCompletionOnlyAfterTerminalToolResult(t *testing.T) {
	for _, kind := range []string{"recoverable", "legacy_unknown", "still_running"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newExecutionFixture(t)
			for _, eventKind := range []string{"tool_start", "tool_execute"} {
				if _, err := f.s.Append(ctx, receiptEvent(f.req, eventKind, "call", "execute")); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "still_running" {
				e := receiptEvent(f.req, "tool_end", "call", "execute")
				e.Status, e.Text = "failed", "unconfirmed external result"
				e.Receipt = &harness.ToolReceipt{AgentRecoverable: kind == "recoverable"}
				if _, err := f.s.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
				state, err := f.s.EndExecutionAttemptTx(ctx, tx, f.lease, harness.ExecutionStopCompleted, "agent ended the turn after processing the error")
				if err == nil && state.Status != harness.ExecutionCompleted {
					t.Fatalf("state=%+v", state)
				}
				if err != nil {
					return err
				}
				return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeCompleted)
			})
			if kind == "recoverable" && err != nil || kind != "recoverable" && !errors.Is(err, harness.ErrReceiptConflict) {
				t.Fatalf("completion %s: %v", kind, err)
			}
		})
	}
}

func TestAgentRecoveryAllowsGoalEvaluationAfterVerifiedRepair(t *testing.T) {
	ctx := context.Background()
	s, req := receiptFixture(t)
	if _, err := s.Store.db.Exec(`INSERT INTO harness_goals(session_id,id,objective) VALUES(?,'goal','verify the repaired result')`, req.Session.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := s.Store.GoalForRun(ctx, req)
	if err != nil || attempt == nil {
		t.Fatal("goal unavailable", err)
	}
	for _, call := range []string{"uncertain", "verify"} {
		for _, kind := range []string{"tool_start", "tool_execute"} {
			appendReceiptEvent(t, s, req, kind, call, "execute")
		}
		e := receiptEvent(req, "tool_end", call, "execute")
		if call == "uncertain" {
			e.Status, e.Text = "failed", "external result was not confirmed"
			e.Receipt = &harness.ToolReceipt{AgentRecoverable: true}
		} else {
			e.Content = []harness.Content{{Type: "text", Text: "independent read verified the requested output"}}
		}
		if _, err = s.Store.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	verdict := harness.GoalEvaluation{Status: "achieved", Reason: "later inspection verified the result", Evidence: []string{"verify"}}
	if err = s.Store.RecordGoalEvaluation(ctx, req, *attempt, verdict, "verified", false); err != nil {
		t.Fatal("recoverable error permanently blocked goal evaluation", err)
	}
	if err = s.Store.Finish(ctx, req.RunID, "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	goal, err := s.Store.Goal(ctx, req.Session.ID)
	if err != nil || goal == nil || goal.Status != "achieved" {
		t.Fatal("verified goal could not finish", goal, err)
	}
	r, err := readReceipt(ctx, s.Store.db, req.Session.ID, req.RunID, "uncertain")
	if err != nil || r.State != harness.ReceiptUncertain || r.Review != nil {
		t.Fatal("goal completion rewrote original uncertainty", r, err)
	}
}

func legacyFaultFixture(t *testing.T) (*executionFixture, *Service, harness.ExecutionState) {
	t.Helper()
	f := newExecutionFixture(t)
	f.pending(t)
	state := f.suspend(t)
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE harness_executions SET status='needs_reconciliation',blocked_reason='original unknown outcome' WHERE run_id=?`, f.req.RunID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE harness_runs SET status='needs_reconciliation',error='original unknown outcome' WHERE id=?`, f.req.RunID); err != nil {
			return err
		}
		r, err := readReceipt(context.Background(), tx, f.req.Session.ID, f.req.RunID, "write")
		if err != nil {
			return err
		}
		r.State, r.Error = harness.ReceiptUncertain, "original unknown outcome"
		return writeReceipt(context.Background(), tx, r)
	})
	svc := NewService(f.s, nil, "")
	if _, err := svc.Coordinator.Attach(f.req.Session.ID, "maintenance"); err != nil {
		t.Fatal(err)
	}
	state.Status = harness.ExecutionNeedsReconciliation
	return f, svc, state
}

func TestCloseFaultedExecutionIsExactAndPreservesEvidence(t *testing.T) {
	f, svc, state := legacyFaultFixture(t)
	ctx := context.Background()
	if err := f.s.ReconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	unchanged, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil || unchanged.Status != harness.ExecutionNeedsReconciliation || unchanged.Version != state.Version {
		t.Fatal("startup silently migrated a legacy fault", unchanged, err)
	}
	other, err := svc.NewSession(ctx, "other", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherReq := harness.RunRequest{Session: other, RunID: "other-legacy", InputID: "other-input"}
	if err = f.s.BeginRun(ctx, otherReq); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`UPDATE harness_runs SET status='needs_reconciliation',error='untouched' WHERE id=?`, otherReq.RunID); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err = f.s.db.QueryRow(`SELECT receipt FROM harness_tool_receipts WHERE run_id=?`, f.req.RunID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	request := CloseFaultedExecutionRequest{RunID: f.req.RunID, ExpectedVersion: state.Version}
	for _, bad := range []CloseFaultedExecutionRequest{{RunID: request.RunID, ExpectedVersion: request.ExpectedVersion + 1}, {RunID: "missing", ExpectedVersion: request.ExpectedVersion}} {
		if _, err = svc.CloseFaultedExecution(ctx, "maintenance", f.req.Session.ID, bad); err == nil {
			t.Fatal("stale or foreign target accepted")
		}
	}
	if _, err = svc.CloseFaultedExecution(ctx, "other", other.ID, request); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("foreign session accepted", err)
	}
	closed, err := svc.CloseFaultedExecution(ctx, "maintenance", f.req.Session.ID, request)
	if err != nil || closed.Status != harness.ExecutionFailed || closed.Version != state.Version+1 || closed.BlockedReason != "original unknown outcome" {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	var after []byte
	var checkpoints, audits int
	var otherStatus, otherError string
	if err = f.s.db.QueryRow(`SELECT receipt FROM harness_tool_receipts WHERE run_id=?`, f.req.RunID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("unknown receipt was rewritten", err)
	}
	if err = f.s.db.QueryRow(`SELECT count(*) FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+f.req.RunID).Scan(&checkpoints); err != nil || checkpoints != 0 {
		t.Fatal("old checkpoint survived", checkpoints, err)
	}
	if err = f.s.db.QueryRow(`SELECT status,error FROM harness_runs WHERE id=?`, otherReq.RunID).Scan(&otherStatus, &otherError); err != nil || otherStatus != "needs_reconciliation" || otherError != "untouched" {
		t.Fatal("unrelated historical run changed", err)
	}
	if err = f.s.db.QueryRow(`SELECT count(*) FROM harness_execution_audits WHERE run_id=? AND kind='faulted_execution_closed' AND json_extract(data,'$.effectsVerified')=0 AND json_extract(data,'$.automaticReplay')=0`, f.req.RunID).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("explicit recovery audit missing", audits, err)
	}
	if err = f.s.requireReconciled(ctx, f.req.Session.ID); err != nil {
		t.Fatal("fresh turn remains locked", err)
	}
	if err = executionTx(ctx, f.s, func(tx *sql.Tx) error {
		_, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "maintenance", harness.ResumeExecutionRequest{RunID: request.RunID, ExpectedVersion: closed.Version}, NewID())
		return err
	}); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("old execution resumed", err)
	}
	if _, err = svc.CloseFaultedExecution(ctx, "maintenance", f.req.Session.ID, request); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("recovery accepted stale version twice", err)
	}
}

func TestCloseFaultedExecutionRefusesActiveOrUnknownBudget(t *testing.T) {
	for _, active := range []string{"running_attempt", "active", "unknown"} {
		t.Run(active, func(t *testing.T) {
			f, svc, state := legacyFaultFixture(t)
			query := `UPDATE budget_attempts SET state=? WHERE id=?`
			value := active
			if active == "running_attempt" {
				query, value = `UPDATE harness_execution_attempts SET status=? WHERE id=?`, "running"
			}
			if _, err := f.s.db.Exec(query, value, state.AttemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CloseFaultedExecution(context.Background(), "maintenance", f.req.Session.ID, CloseFaultedExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}); !errors.Is(err, harness.ErrBusy) {
				t.Fatal("active accounting was waived", err)
			}
		})
	}
}

func TestSessionCleanupPreservesTerminalUnknownUntilExplicitDelete(t *testing.T) {
	for _, tc := range []struct {
		status  string
		state   harness.ReceiptState
		allowed bool
	}{
		{"completed", harness.ReceiptUncertain, true},
		{"failed", harness.ReceiptUncertain, true},
		{"cancelled", harness.ReceiptUncertain, true},
		{"running", harness.ReceiptUncertain, false},
		{"failed", harness.ReceiptStarted, false},
		{"failed", harness.ReceiptPending, false},
	} {
		t.Run(tc.status+"/"+string(tc.state), func(t *testing.T) {
			ctx := context.Background()
			s, req := receiptFixture(t)
			appendReceiptEvent(t, s, req, "tool_start", "effect", "execute")
			if tc.state != harness.ReceiptPending {
				appendReceiptEvent(t, s, req, "tool_execute", "effect", "execute")
			}
			if tc.state == harness.ReceiptUncertain {
				e := receiptEvent(req, "tool_end", "effect", "execute")
				e.Status, e.Text = "failed", "effect remains unconfirmed"
				if _, err := s.Store.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status=? WHERE id=?`, tc.status, req.RunID); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().AddDate(0, 0, -31).Format(time.RFC3339Nano)
			if _, err := s.Store.db.Exec(`UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, req.Session.ID); err != nil {
				t.Fatal(err)
			}
			var before []byte
			if err := s.Store.db.QueryRow(`SELECT receipt FROM harness_tool_receipts WHERE run_id=?`, req.RunID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Hour}
			eligible, err := s.Store.IsCleanupEligible(ctx, req.Session.ID, time.Now().UTC(), policy)
			if err != nil || eligible != tc.allowed {
				t.Fatal("retention eligibility differs from execution state", eligible, err)
			}
			archived, _, err := s.Store.archiveSession(ctx, req.Session.ID, time.Now().UTC(), policy)
			if tc.allowed && (err != nil || !archived) || !tc.allowed && (err == nil || archived) {
				t.Fatal("archive ignored execution boundary", archived, err)
			}
			var after []byte
			if err := s.Store.db.QueryRow(`SELECT receipt FROM harness_tool_receipts WHERE run_id=?`, req.RunID).Scan(&after); err != nil || !bytes.Equal(before, after) {
				t.Fatal("archiving changed effect evidence", err)
			}
			_, _, err = s.Store.purgeSession(ctx, req.Session.ID, false)
			if (err == nil) != tc.allowed {
				t.Fatal("explicit deletion ignored execution boundary", err)
			}
			if tc.allowed {
				if _, err := s.Store.Session(ctx, req.Session.ID); !errors.Is(err, harness.ErrNotFound) {
					t.Fatal("explicit delete retained session", err)
				}
			}
		})
	}
}

func TestTerminalUnknownCleanupStillChecksExecutionAndBudgetAttempts(t *testing.T) {
	for _, blocker := range []string{"execution_attempt", "budget_attempt"} {
		t.Run(blocker, func(t *testing.T) {
			f, svc, state := legacyFaultFixture(t)
			ctx := context.Background()
			if _, err := svc.CloseFaultedExecution(ctx, "maintenance", f.req.Session.ID, CloseFaultedExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}); err != nil {
				t.Fatal(err)
			}
			query := `UPDATE harness_execution_attempts SET status='running' WHERE id=?`
			if blocker == "budget_attempt" {
				query = `UPDATE budget_attempts SET state='active' WHERE id=?`
			}
			if _, err := f.s.db.Exec(query, state.AttemptID); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().AddDate(0, 0, -31).Format(time.RFC3339Nano)
			if _, err := f.s.db.Exec(`UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, f.req.Session.ID); err != nil {
				t.Fatal(err)
			}
			policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Hour}
			if archived, _, err := f.s.archiveSession(ctx, f.req.Session.ID, time.Now().UTC(), policy); !errors.Is(err, harness.ErrBusy) || archived {
				t.Fatal("active attempt archived", archived, err)
			}
			if _, _, err := f.s.purgeSession(ctx, f.req.Session.ID, false); !errors.Is(err, harness.ErrBusy) {
				t.Fatal("active attempt deleted", err)
			}
		})
	}
}

func TestTerminalUnknownBackgroundGraphCanBeArchivedAndDeleted(t *testing.T) {
	f := completedGraphFixture(t)
	ctx := context.Background()
	if _, err := f.native.DB().Exec(`INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,'unknown-effect',?,'uncertain',1,?)`, f.binding.TaskID, f.binding.ChildSessionID, []byte(`{"state":"uncertain","error":"effects unconfirmed"}`)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -31).Format(time.RFC3339Nano)
	if _, err := f.native.DB().Exec(`UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, f.binding.ParentSessionID); err != nil {
		t.Fatal(err)
	}
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Hour}
	if archived, _, err := f.store.archiveSession(ctx, f.binding.ParentSessionID, time.Now().UTC(), policy); err != nil || !archived {
		t.Fatal("terminal child uncertainty prevented archive", archived, err)
	}
	var unknown int
	if err := f.native.DB().QueryRow(`SELECT count(*) FROM harness_tool_receipts WHERE run_id=? AND state='uncertain'`, f.binding.TaskID).Scan(&unknown); err != nil || unknown != 1 {
		t.Fatal("archive erased child evidence", unknown, err)
	}
	if _, _, err := f.store.purgeSession(ctx, f.binding.ParentSessionID, false); err != nil {
		t.Fatal("terminal child uncertainty prevented explicit deletion", err)
	}
}
