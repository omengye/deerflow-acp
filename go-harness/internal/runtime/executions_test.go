package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

type executionFixture struct {
	s          *Store
	ledger     *budget.Ledger
	req        harness.RunRequest
	lease      ExecutionLease
	intent     harness.ExecutionInteraction
	permission harness.PermissionRequest
}

func executionTx(ctx context.Context, s *Store, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = f(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func mustExecutionTx(t *testing.T, f *executionFixture, fn func(*sql.Tx) error) {
	t.Helper()
	if err := executionTx(context.Background(), f.s, fn); err != nil {
		t.Fatal(err)
	}
}
func newExecutionFixture(t *testing.T) *executionFixture {
	t.Helper()
	ctx := context.Background()
	service := newSessionTestService(t)
	if err := migrateExecutions(ctx, service.Store.db); err != nil {
		t.Fatal(err)
	}
	x, err := service.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := NewID()
	req := harness.RunRequest{Session: x, RunID: run, RootBudgetID: run, InputID: NewID(), Input: []harness.Content{{Type: "text", Text: "original prompt"}}}
	if err = service.Store.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	ledger, err := budget.New(service.Store.db, budget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f := &executionFixture{s: service.Store, ledger: ledger, req: req}
	scope := budget.Scope{RootBudgetID: run, MemberID: run, SessionID: x.ID, AttemptID: NewID(), Fence: 1}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		if err := ledger.CreateRootTx(ctx, tx, budget.RootSpec{RootBudgetID: run, SessionID: x.ID, RootRunID: run, Limits: harness.BudgetLimits{MaxModelCalls: 20, MaxToolCalls: 20, MaxTokens: 10000, MaxOutputTokens: 1000}}); err != nil {
			return err
		}
		if err := ledger.BeginAttemptTx(ctx, tx, scope); err != nil {
			return err
		}
		var err error
		f.lease, err = f.s.BeginExecutionTx(ctx, tx, req, "owner", scope)
		return err
	})
	return f
}
func (f *executionFixture) pending(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	e := harness.RunEvent{SessionID: f.req.Session.ID, RunID: f.req.RunID, Kind: "tool_start", ToolCallID: "write", ToolName: "write_file", Status: "pending", Arguments: json.RawMessage(`{"path":"report.txt","content":"approved content"}`)}
	if _, err := f.s.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	f.permission = harness.PermissionRequest{SessionID: f.req.Session.ID, RunID: f.req.RunID, ConfigVersion: f.req.Session.ConfigVersion, ToolCallID: e.ToolCallID, ToolName: e.ToolName, Arguments: e.Arguments}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		var err error
		f.intent, err = f.s.RecordPermissionIntentTx(ctx, tx, f.lease, f.permission)
		return err
	})
	f.permission.ID = f.intent.ID
}
func (f *executionFixture) checkpoint() ExecutionCheckpoint {
	return ExecutionCheckpoint{ID: "harness/turn/v1/" + f.req.RunID, Data: []byte("trusted native checkpoint fixture"), Interrupts: []ExecutionInterruptBinding{{IntentID: f.intent.ID, IntentVersion: f.intent.Version, NativeInterruptID: "native/tool/write"}}}
}
func (f *executionFixture) suspend(t *testing.T) harness.ExecutionState {
	t.Helper()
	ctx := context.Background()
	var state harness.ExecutionState
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		var err error
		state, err = f.s.SuspendExecutionTx(ctx, tx, f.lease, f.checkpoint())
		if err != nil {
			return err
		}
		return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeWaitingInput)
	})
	return state
}
func (f *executionFixture) resume(t *testing.T, owner string) ExecutionLease {
	t.Helper()
	ctx := context.Background()
	state, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var lease ExecutionLease
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		var err error
		lease, err = f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, owner, harness.ResumeExecutionRequest{RunID: f.req.RunID, ExpectedVersion: state.Version}, NewID())
		if err != nil {
			return err
		}
		return f.ledger.BeginAttemptTx(ctx, tx, lease.Scope)
	})
	return lease
}
func (f *executionFixture) grant(t *testing.T, lease ExecutionLease) PermissionGrant {
	t.Helper()
	var grant PermissionGrant
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		var err error
		grant, err = f.s.RecordPermissionGrantTx(context.Background(), tx, lease, f.intent.ID, f.intent.Version, harness.AllowOnce)
		return err
	})
	return grant
}
func (f *executionFixture) dispatch(tx *sql.Tx, lease ExecutionLease, grant PermissionGrant) error {
	ctx := context.Background()
	decision, err := f.s.ConsumePermissionGrantTx(ctx, tx, lease, grant, f.permission)
	if err != nil {
		return err
	}
	if decision != harness.AllowOnce {
		return harness.ErrPermissionDenied
	}
	receipt, err := readReceipt(ctx, tx, f.req.Session.ID, f.req.RunID, f.permission.ToolCallID)
	if err != nil {
		return err
	}
	receipt.State = harness.ReceiptStarted
	receipt.Version++
	if err = writeReceipt(ctx, tx, receipt); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, harness.RunEvent{SessionID: f.req.Session.ID, RunID: f.req.RunID, Kind: "tool_execute", ToolCallID: f.permission.ToolCallID, ToolName: f.permission.ToolName, Status: "in_progress", Receipt: &receipt})
	return err
}

func TestExecutionWaitingResumePreservesIdentityAndDispatchesOnce(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		x, err := f.s.RecordPermissionIntentTx(ctx, tx, f.lease, f.permission)
		if err == nil && x.ID != f.intent.ID {
			t.Error("intent identity changed")
		}
		return err
	})
	waiting := f.suspend(t)
	if waiting.Status != harness.ExecutionWaitingInput || !waiting.Resumable {
		t.Fatalf("state=%+v", waiting)
	}
	if err := f.s.requireNoWaitingExecution(ctx, f.req.Session.ID); !errors.Is(err, harness.ErrExecutionWaitingInput) {
		t.Fatal("new prompt not blocked:", err)
	}
	state, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.WaitingInputs) != 1 || state.WaitingInputs[0].ArgumentsDigest == "" {
		t.Fatal("missing pending interaction")
	}
	public, _ := json.Marshal(state)
	if strings.Contains(string(public), "native/tool") || strings.Contains(string(public), "checkpoint") || strings.Contains(string(public), "approved content") {
		t.Fatal("internal execution state exposed")
	}
	lease := f.resume(t, "new-owner")
	grant := f.grant(t, lease)
	if lease.Scope.MemberID != f.req.RunID || lease.InputID != f.req.InputID || lease.Scope.RootBudgetID != f.lease.Scope.RootBudgetID || lease.Scope.Fence != 2 || lease.Scope.AttemptID == f.lease.Scope.AttemptID {
		t.Fatal("resume changed logical identity")
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		req, err := f.s.ExecutionRequestTx(ctx, tx, lease)
		if err != nil {
			return err
		}
		if req.RunID != f.req.RunID || req.InputID != f.req.InputID || req.RootBudgetID != f.req.RootBudgetID || req.Input[0].Text != "original prompt" {
			t.Error("request replaced accepted input")
		}
		bindings, err := f.s.ExecutionResumeBindingsTx(ctx, tx, lease)
		if err == nil && (len(bindings) != 1 || bindings[0].Grant.ID != grant.ID) {
			t.Error("broker did not bind granted native target")
		}
		return err
	})
	mustExecutionTx(t, f, func(tx *sql.Tx) error { return f.dispatch(tx, lease, grant) })
	if err := executionTx(ctx, f.s, func(tx *sql.Tx) error { return f.dispatch(tx, lease, grant) }); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("grant replay accepted:", err)
	}
	if _, err = f.s.Append(ctx, harness.RunEvent{SessionID: f.req.Session.ID, RunID: f.req.RunID, Kind: "tool_end", ToolCallID: "write", ToolName: "write_file", Status: "completed", Content: []harness.Content{{Type: "text", Text: "written"}}}); err != nil {
		t.Fatal(err)
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		_, err := f.s.EndExecutionAttemptTx(ctx, tx, lease, harness.ExecutionStopCompleted, "")
		if err != nil {
			return err
		}
		return f.ledger.EndAttemptTx(ctx, tx, lease.Scope, budget.OutcomeCompleted)
	})
	state, err = f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil || state.Status != harness.ExecutionCompleted || state.Resumable {
		t.Fatalf("terminal=%+v %v", state, err)
	}
	var count int
	if err = f.s.db.QueryRow(`SELECT count(*) FROM eino_checkpoints WHERE id=?`, f.checkpoint().ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("completed checkpoint executable")
	}
}

func TestExecutionSuspensionAndGrantConsumptionRollbackAtomically(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_wait_audit BEFORE INSERT ON harness_execution_audits WHEN NEW.kind='waiting_input' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
		_, err := f.s.SuspendExecutionTx(ctx, tx, f.lease, f.checkpoint())
		if err != nil {
			return err
		}
		return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeWaitingInput)
	})
	if err == nil {
		t.Fatal("suspension fault ignored")
	}
	var state, attempt, budgetState string
	var cp int
	if err = f.s.db.QueryRow(`SELECT e.status,a.status,b.state FROM harness_executions e JOIN harness_execution_attempts a ON a.id=e.attempt_id JOIN budget_attempts b ON b.id=a.id WHERE e.run_id=?`, f.req.RunID).Scan(&state, &attempt, &budgetState); err != nil {
		t.Fatal(err)
	}
	if state != "suspending" || attempt != "running" || budgetState != "active" {
		t.Fatal("suspension partially committed")
	}
	if err = f.s.db.QueryRow(`SELECT count(*) FROM eino_checkpoints`).Scan(&cp); err != nil || cp != 0 {
		t.Fatal("orphan checkpoint")
	}
	if _, err = f.s.db.Exec(`DROP TRIGGER fail_wait_audit`); err != nil {
		t.Fatal(err)
	}
	f.suspend(t)
	lease := f.resume(t, "new-owner")
	grant := f.grant(t, lease)
	if _, err = f.s.db.Exec(`CREATE TRIGGER fail_execute BEFORE UPDATE ON harness_tool_receipts WHEN NEW.state='started' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err = executionTx(ctx, f.s, func(tx *sql.Tx) error { return f.dispatch(tx, lease, grant) }); err == nil {
		t.Fatal("execute fault ignored")
	}
	var grantState, intentState string
	if err = f.s.db.QueryRow(`SELECT g.state,i.state FROM harness_execution_grants g JOIN harness_execution_intents i ON i.id=g.intent_id WHERE g.id=?`, grant.ID).Scan(&grantState, &intentState); err != nil || grantState != "ready" || intentState != "pending" {
		t.Fatal("grant consumption not atomic with receipt")
	}
	if _, err = f.s.db.Exec(`DROP TRIGGER fail_execute`); err != nil {
		t.Fatal(err)
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error { return f.dispatch(tx, lease, grant) })
}

func TestExecutionDisconnectRevokesGrantAndRequiresFreshAuthorization(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	f.suspend(t)
	lease := f.resume(t, "connection-two")
	grant := f.grant(t, lease)
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		x, err := f.s.EndExecutionAttemptTx(ctx, tx, lease, harness.ExecutionStopDisconnected, "EOF")
		if err != nil {
			return err
		}
		if x.Status != harness.ExecutionWaitingInput {
			t.Error("disconnect lost waiting state")
		}
		return f.ledger.EndAttemptTx(ctx, tx, lease.Scope, budget.OutcomeWaitingInput)
	})
	if err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
		_, err := f.s.RecordPermissionGrantTx(ctx, tx, lease, f.intent.ID, f.intent.Version, harness.AllowAlways)
		return err
	}); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("old connection authorized after detach:", err)
	}
	next := f.resume(t, "connection-three")
	if err := executionTx(ctx, f.s, func(tx *sql.Tx) error { return f.dispatch(tx, next, grant) }); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("old grant valid in new attempt:", err)
	}
	if err := executionTx(ctx, f.s, func(tx *sql.Tx) error { _, err := f.s.ExecutionResumeBindingsTx(ctx, tx, next); return err }); !errors.Is(err, harness.ErrExecutionWaitingInput) {
		t.Fatal("resume bypassed fresh approval:", err)
	}
	fresh := f.grant(t, next)
	mustExecutionTx(t, f, func(tx *sql.Tx) error { return f.dispatch(tx, next, fresh) })
}

func TestExecutionResumeRejectsCheckpointInputNativeAndReceiptDrift(t *testing.T) {
	for _, kind := range []string{"checkpoint", "input", "config", "native", "business", "receipt", "started", "binding"} {
		t.Run(kind, func(t *testing.T) {
			f := newExecutionFixture(t)
			ctx := context.Background()
			f.pending(t)
			waiting := f.suspend(t)
			var err error
			switch kind {
			case "checkpoint":
				_, err = f.s.db.Exec(`UPDATE eino_checkpoints SET payload=? WHERE id=?`, []byte("changed"), f.checkpoint().ID)
			case "input":
				_, err = f.s.db.Exec(`UPDATE harness_inputs SET content='[]' WHERE id=?`, f.req.InputID)
			case "config":
				_, err = f.s.db.Exec(`UPDATE harness_session_configs SET version=version+1 WHERE session_id=?`, f.req.Session.ID)
			case "native":
				_, err = f.s.db.Exec(`INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,?,'message','{}')`, f.req.Session.ID, NewID())
			case "business":
				_, err = f.s.Append(ctx, harness.RunEvent{SessionID: f.req.Session.ID, RunID: f.req.RunID, Kind: "text_delta", Text: "later event"})
			case "receipt":
				_, err = f.s.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE run_id=?`, f.req.RunID)
			case "started":
				_, err = f.s.db.Exec(`UPDATE harness_tool_receipts SET state='started' WHERE run_id=?`, f.req.RunID)
			case "binding":
				_, err = f.s.db.Exec(`UPDATE harness_execution_intents SET version=version+1 WHERE run_id=?`, f.req.RunID)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = executionTx(ctx, f.s, func(tx *sql.Tx) error {
				_, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "new", harness.ResumeExecutionRequest{RunID: f.req.RunID, ExpectedVersion: waiting.Version}, NewID())
				return err
			})
			if !errors.Is(err, harness.ErrExecutionUnresumable) && !errors.Is(err, harness.ErrExecutionConflict) && !errors.Is(err, harness.ErrReconciliationRequired) {
				t.Fatal("drift accepted:", err)
			}
			var attempts int
			if err = f.s.db.QueryRow(`SELECT count(*) FROM harness_execution_attempts WHERE run_id=?`, f.req.RunID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatal("rejected resume created attempt")
			}
		})
	}
}

func TestExecutionResumeCASAndCancellationPreventReplay(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	waiting := f.suspend(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- executionTx(ctx, f.s, func(tx *sql.Tx) error {
				lease, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, fmt.Sprint("owner", i), harness.ResumeExecutionRequest{RunID: f.req.RunID, ExpectedVersion: waiting.Version}, NewID())
				if err != nil {
					return err
				}
				return f.ledger.BeginAttemptTx(ctx, tx, lease.Scope)
			})
		}(i)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, harness.ErrExecutionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("concurrent resumes both entered")
	}
	g := newExecutionFixture(t)
	g.pending(t)
	state := g.suspend(t)
	mustExecutionTx(t, g, func(tx *sql.Tx) error {
		_, err := g.s.CancelExecutionTx(ctx, tx, g.req.Session.ID, "new-owner", harness.CancelExecutionRequest{RunID: g.req.RunID, ExpectedVersion: state.Version})
		return err
	})
	err := executionTx(ctx, g.s, func(tx *sql.Tx) error {
		_, err := g.s.ClaimExecutionResumeTx(ctx, tx, g.req.Session.ID, "new-owner", harness.ResumeExecutionRequest{RunID: g.req.RunID, ExpectedVersion: state.Version}, NewID())
		return err
	})
	if !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("cancelled execution resumed:", err)
	}
	receipt, err := readReceipt(ctx, g.s.db, g.req.Session.ID, g.req.RunID, "write")
	if err != nil || receipt.State != harness.ReceiptNotExecuted {
		t.Fatal("cancel did not settle pending receipt")
	}
	if err = g.s.requireNoWaitingExecution(ctx, g.req.Session.ID); err != nil {
		t.Fatal("cancel did not unblock new input")
	}
}

func TestExecutionStartupPreservesValidWaitAndBlocksAdvancedFailedAttempt(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprint(advanced), func(t *testing.T) {
			f := newExecutionFixture(t)
			ctx := context.Background()
			f.pending(t)
			f.suspend(t)
			lease := f.resume(t, "disconnected")
			grant := f.grant(t, lease)
			if advanced {
				mustExecutionTx(t, f, func(tx *sql.Tx) error { return f.dispatch(tx, lease, grant) })
			}
			mustExecutionTx(t, f, func(tx *sql.Tx) error {
				recovered, err := f.s.RecoverExecutionsTx(ctx, tx)
				if err != nil {
					return err
				}
				if len(recovered) != 1 || !recovered[0].AttemptWasRunning {
					t.Error("recovery scope missing")
				}
				return f.ledger.EndAttemptTx(ctx, tx, lease.Scope, budget.OutcomeInterrupted)
			})
			state, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
			if err != nil {
				t.Fatal(err)
			}
			want := harness.ExecutionWaitingInput
			if advanced {
				want = harness.ExecutionNeedsReconciliation
			}
			if state.Status != want || state.Resumable == advanced {
				t.Fatalf("recovered=%+v", state)
			}
			var grantState string
			if err = f.s.db.QueryRow(`SELECT state FROM harness_execution_grants WHERE id=?`, grant.ID).Scan(&grantState); err != nil {
				t.Fatal(err)
			}
			if !advanced && grantState != "revoked" {
				t.Fatal("recovered attempt retained live authorization")
			}
			if advanced {
				receipt, err := readReceipt(ctx, f.s.db, f.req.Session.ID, f.req.RunID, "write")
				if err != nil || receipt.State != harness.ReceiptUncertain {
					t.Fatal("interrupted effect lost uncertainty")
				}
			}
		})
	}
}

func TestExecutionManifestCapturesNativeHeadAndRequiresAllPendingCalls(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	if _, err := f.s.db.Exec(`INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,'native-head','message',?)`, f.req.Session.ID, []byte(`{"native":true}`)); err != nil {
		t.Fatal(err)
	}
	e := harness.RunEvent{SessionID: f.req.Session.ID, RunID: f.req.RunID, Kind: "tool_start", ToolCallID: "sibling", ToolName: "write_file", Status: "pending", Arguments: json.RawMessage(`{}`)}
	if _, err := f.s.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error { _, err := f.s.SuspendExecutionTx(ctx, tx, f.lease, f.checkpoint()); return err })
	if !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("unrepresented pending sibling accepted:", err)
	}
	p := harness.PermissionRequest{SessionID: f.req.Session.ID, RunID: f.req.RunID, ConfigVersion: f.req.Session.ConfigVersion, ToolCallID: e.ToolCallID, ToolName: e.ToolName, Arguments: e.Arguments}
	var sibling harness.ExecutionInteraction
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		var err error
		sibling, err = f.s.RecordPermissionIntentTx(ctx, tx, f.lease, p)
		return err
	})
	cp := f.checkpoint()
	cp.Interrupts = append(cp.Interrupts, ExecutionInterruptBinding{IntentID: sibling.ID, IntentVersion: sibling.Version, NativeInterruptID: "native/sibling"})
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		_, err := f.s.SuspendExecutionTx(ctx, tx, f.lease, cp)
		if err != nil {
			return err
		}
		return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeWaitingInput)
	})
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, f.req.Session.ID, f.req.RunID)
		if err == nil && (row.Manifest.NativeHead.EventID != "native-head" || row.Manifest.NativeHead.Sequence < 1 || row.Manifest.NativeHead.Digest != executionDigest([]byte(`{"native":true}`)) || row.Manifest.EventCursor == row.Manifest.NativeHead.Sequence) {
			t.Error("native and business cursors conflated")
		}
		return err
	})
}

func TestExecutionManifestBindsEarlierSummaryReplacement(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	if _, err := f.s.db.Exec(`INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,'summary','messages_replaced',?)`, f.req.Session.ID, []byte(`{"summary":"original"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,'later','message',?)`, f.req.Session.ID, []byte(`{"later":true}`)); err != nil {
		t.Fatal(err)
	}
	f.suspend(t)
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, f.req.Session.ID, f.req.RunID)
		if err != nil {
			return err
		}
		if row.Manifest.SummaryHead.EventID != "summary" || row.Manifest.SummaryHead.Digest != executionDigest([]byte(`{"summary":"original"}`)) {
			t.Fatalf("missing summary binding: %+v", row.Manifest.SummaryHead)
		}
		return validateExecutionManifest(ctx, tx, row)
	})
	if _, err := f.s.db.Exec(`UPDATE eino_session_events SET payload=? WHERE session_id=? AND event_id='summary'`, []byte(`{"summary":"changed"}`), f.req.Session.ID); err != nil {
		t.Fatal(err)
	}
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, f.req.Session.ID, f.req.RunID)
		if err != nil {
			return err
		}
		return validateExecutionManifest(ctx, tx, row)
	})
	if !errors.Is(err, harness.ErrExecutionUnresumable) {
		t.Fatalf("changed summary accepted: %v", err)
	}
}

func TestExecutionBrokerRejectsForgedIdentityAndConfigDrift(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	f.suspend(t)
	lease := f.resume(t, "new-owner")
	grant := f.grant(t, lease)
	for _, kind := range []string{"session", "run", "root", "input", "owner", "attempt", "fence", "tool", "arguments", "grant"} {
		t.Run(kind, func(t *testing.T) {
			forgedLease, forgedGrant, forgedPermission := lease, grant, f.permission
			switch kind {
			case "session":
				forgedLease.Scope.SessionID = "another-session"
			case "run":
				forgedLease.Scope.MemberID = "another-run"
			case "root":
				forgedLease.Scope.RootBudgetID = "another-budget"
			case "input":
				forgedLease.InputID = "another-input"
			case "owner":
				forgedLease.OwnerID = "former-owner"
			case "attempt":
				forgedLease.Scope.AttemptID = f.lease.Scope.AttemptID
			case "fence":
				forgedLease.Scope.Fence++
			case "tool":
				forgedPermission.ToolCallID = "different-call"
			case "arguments":
				forgedPermission.Arguments = json.RawMessage(`{"path":"elsewhere"}`)
			case "grant":
				forgedGrant.Decision = harness.AllowAlways
			}
			err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
				_, err := f.s.ValidatePermissionGrantTx(ctx, tx, forgedLease, forgedGrant, forgedPermission)
				return err
			})
			if !errors.Is(err, harness.ErrExecutionConflict) && !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("forged authorization accepted:", err)
			}
		})
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		_, err := f.s.ValidatePermissionGrantTx(ctx, tx, lease, grant, f.permission)
		return err
	})
	var grantState, intentState string
	if err := f.s.db.QueryRow(`SELECT g.state,i.state FROM harness_execution_grants g JOIN harness_execution_intents i ON i.id=g.intent_id WHERE g.id=?`, grant.ID).Scan(&grantState, &intentState); err != nil || grantState != "ready" || intentState != "pending" {
		t.Fatal("read-only validation consumed authorization")
	}
	if _, err := f.s.db.Exec(`UPDATE harness_session_configs SET version=version+1 WHERE session_id=?`, f.req.Session.ID); err != nil {
		t.Fatal(err)
	}
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
		_, err := f.s.RecordPermissionGrantTx(ctx, tx, lease, f.intent.ID, f.intent.Version, harness.AllowOnce)
		return err
	})
	if !errors.Is(err, harness.ErrExecutionUnresumable) {
		t.Fatal("configuration drift accepted at grant creation:", err)
	}
}

func TestExecutionResumeAndCancelFailuresRollBackWholeLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, point := range []string{"resume_audit", "resume_ledger", "cancel_audit", "cas_no_update"} {
		t.Run(point, func(t *testing.T) {
			f := newExecutionFixture(t)
			f.pending(t)
			waiting := f.suspend(t)
			var trigger string
			switch point {
			case "resume_audit":
				trigger = `CREATE TRIGGER lifecycle_fault BEFORE INSERT ON harness_execution_audits WHEN NEW.kind='attempt_resuming' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			case "resume_ledger":
				trigger = `CREATE TRIGGER lifecycle_fault BEFORE INSERT ON budget_attempts BEGIN SELECT RAISE(ABORT,'fixture'); END`
			case "cancel_audit":
				trigger = `CREATE TRIGGER lifecycle_fault BEFORE INSERT ON harness_execution_audits WHEN NEW.kind='execution_cancelled' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			case "cas_no_update":
				trigger = `CREATE TRIGGER lifecycle_fault BEFORE UPDATE ON harness_executions WHEN NEW.status='resuming' BEGIN SELECT RAISE(IGNORE); END`
			}
			if _, err := f.s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
				if point == "cancel_audit" {
					_, err := f.s.CancelExecutionTx(ctx, tx, f.req.Session.ID, "new-owner", harness.CancelExecutionRequest{RunID: f.req.RunID, ExpectedVersion: waiting.Version})
					return err
				}
				lease, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "new-owner", harness.ResumeExecutionRequest{RunID: f.req.RunID, ExpectedVersion: waiting.Version}, NewID())
				if err != nil {
					return err
				}
				return f.ledger.BeginAttemptTx(ctx, tx, lease.Scope)
			})
			if err == nil {
				t.Fatal("lifecycle persistence failure ignored")
			}
			if point == "cas_no_update" && !errors.Is(err, harness.ErrExecutionConflict) {
				t.Fatal("zero-row compare-and-swap not rejected:", err)
			}
			state, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
			if err != nil || state.Status != harness.ExecutionWaitingInput || state.Version != waiting.Version || state.Attempt != 1 || !state.Resumable {
				t.Fatalf("partial lifecycle commit: %+v %v", state, err)
			}
			var attempts int
			if err = f.s.db.QueryRow(`SELECT count(*) FROM harness_execution_attempts WHERE run_id=?`, f.req.RunID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatal("rolled-back resume retained an attempt")
			}
			receipt, err := readReceipt(ctx, f.s.db, f.req.Session.ID, f.req.RunID, "write")
			if err != nil || receipt.State != harness.ReceiptPending {
				t.Fatal("rolled-back cancel settled pending receipt")
			}
		})
	}
}

func TestExecutionFailedResumeKeepsCheckpointAndSpentBudget(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	f.pending(t)
	f.suspend(t)
	lease := f.resume(t, "retry-owner")
	reservation, err := f.ledger.ReserveModel(ctx, lease.Scope, budget.ModelRequest{OperationID: NewID(), Digest: "failed-resume-model", InputTokens: 100, MaxOutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := f.ledger.MarkDispatched(ctx, reservation); err != nil || !first {
		t.Fatal("model dispatch failed:", err)
	}
	if _, err = f.ledger.Settle(ctx, reservation, budget.Settlement{Complete: false}); err != nil {
		t.Fatal(err)
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		state, err := f.s.EndExecutionAttemptTx(ctx, tx, lease, harness.ExecutionStopFailed, "model failed before session/effect commit")
		if err != nil {
			return err
		}
		if state.Status != harness.ExecutionWaitingInput {
			t.Error("unchanged checkpoint lost after failed attempt")
		}
		return f.ledger.EndAttemptTx(ctx, tx, lease.Scope, budget.OutcomeWaitingInput)
	})
	next := f.resume(t, "fresh-owner")
	if next.Scope.Fence != 3 {
		t.Fatal("failed attempt fence reused")
	}
	snapshot, err := f.ledger.Snapshot(ctx, next.Scope.RootBudgetID)
	if err != nil || snapshot.SpentTokens != 200 || snapshot.HeldTokens != 0 || snapshot.ModelCalls != 1 {
		t.Fatalf("failed resume refunded budget: %+v %v", snapshot, err)
	}
	var checkpoint []byte
	if err = f.s.db.QueryRow(`SELECT payload FROM eino_checkpoints WHERE id=?`, f.checkpoint().ID).Scan(&checkpoint); err != nil || string(checkpoint) != string(f.checkpoint().Data) {
		t.Fatal("failed resume rewrote checkpoint")
	}
}
