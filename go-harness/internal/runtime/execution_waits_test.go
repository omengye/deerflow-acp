package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type executionWaitFixture struct {
	*executionFixture
	child *backgroundHostFixture
	wait  interaction.TaskWaitBinding
}

func newExecutionWaitFixture(t *testing.T, configure ...func(*backgroundHostFixture)) *executionWaitFixture {
	t.Helper()
	ctx := context.Background()
	child := newBackgroundHostFixture(t)
	for _, apply := range configure {
		apply(child)
	}
	if err := child.create(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.store.Append(ctx, harness.RunEvent{SessionID: child.binding.ParentSessionID, RunID: child.binding.OriginRunID, Kind: "tool_end", ToolCallID: "delegate", ToolName: "background_agent", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: child.spec.Parent, RunID: child.binding.OriginRunID, RootBudgetID: child.binding.RootBudgetID, Input: []harness.Content{{Type: "text", Text: "parent goal"}}}
	if err := child.store.db.QueryRow(`SELECT input_id FROM harness_runs WHERE id=?`, req.RunID).Scan(&req.InputID); err != nil {
		t.Fatal(err)
	}
	f := &executionFixture{s: child.store, ledger: child.store.BudgetLedger, req: req}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		var err error
		f.lease, err = f.s.BeginExecutionTx(ctx, tx, req, "owner", foregroundBudgetScope(req))
		return err
	})
	return &executionWaitFixture{executionFixture: f, child: child, wait: interaction.TaskWaitBinding{NativeInterruptID: "native/wait", Wait: interaction.TaskWait{TaskID: child.binding.TaskID, AfterVersion: 1, CallID: "wait", SessionID: req.Session.ID, RunID: req.RunID, RootBudgetID: req.RootBudgetID}}}
}

func (f *executionWaitFixture) checkpoint(t *testing.T, permissions bool) ExecutionCheckpoint {
	t.Helper()
	cp := ExecutionCheckpoint{ID: "harness/turn/v1/" + f.req.RunID, TaskWaits: []interaction.TaskWaitBinding{f.wait}}
	if permissions {
		cp.Interrupts = f.executionFixture.checkpoint().Interrupts
	}
	var err error
	cp.Data, err = json.Marshal(map[string]any{"taskWaitsEnabled": true, "taskWaits": cp.TaskWaits, "native": "opaque fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func (f *executionWaitFixture) suspend(t *testing.T, permissions bool) harness.ExecutionState {
	t.Helper()
	ctx := context.Background()
	cp := f.checkpoint(t, permissions)
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
		if _, err := f.s.SuspendExecutionTx(ctx, tx, f.lease, cp); err != nil {
			return err
		}
		return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeWaitingInput)
	})
	state, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *executionWaitFixture) complete(t *testing.T) {
	t.Helper()
	f.child.start(t)
	if err := f.child.begin(f.child.scope); err != nil {
		t.Fatal(err)
	}
	task, err := f.child.tasks.Get(context.Background(), f.child.binding.TaskID)
	if err == nil {
		_, err = f.child.tasks.Complete(context.Background(), &bt.CompleteTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version})
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecutionTaskWaitReadinessRecoveryAndClaim(t *testing.T) {
	f := newExecutionWaitFixture(t)
	state := f.suspend(t, false)
	if state.Resumable || state.BlockedReason != "" || len(state.WaitingInputs) != 0 || len(state.WaitingTasks) != 1 || state.WaitingTasks[0] != f.child.binding.TaskID {
		t.Fatalf("wait projected as approval: %+v", state)
	}
	ctx := context.Background()
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
		_, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
		return err
	})
	if !errors.Is(err, harness.ErrExecutionWaitingInput) {
		t.Fatalf("premature resume: %v", err)
	}
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
		recovered, err := f.s.RecoverExecutionsTx(ctx, tx)
		if err == nil && (len(recovered) != 1 || recovered[0].Status != harness.ExecutionWaitingInput || recovered[0].AttemptWasRunning) {
			t.Fatalf("recovery lost safe wait: %+v", recovered)
		}
		return err
	})
	f.complete(t)
	ready, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil || !ready.Resumable || ready.Version != state.Version {
		t.Fatalf("ready projection: %+v %v", ready, err)
	}
	lease := f.resume(t, "new-owner")
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
		row, err := checkExecutionLease(ctx, tx, lease)
		if err != nil {
			return err
		}
		targets, err := executionWaitTargets(ctx, tx, row)
		if err == nil && (len(targets) != 1 || targets[f.wait.NativeInterruptID] != f.wait.Wait || lease.Scope.RootBudgetID != f.req.RootBudgetID || lease.InputID != f.req.InputID) {
			t.Fatalf("resume identity changed: %+v %+v", lease, targets)
		}
		return err
	})
	err = executionTx(ctx, f.s, func(tx *sql.Tx) error {
		_, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
		return err
	})
	if !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("duplicate wait claim: %v", err)
	}
}

func TestExecutionTaskWaitRejectsForgedScopeAndManifest(t *testing.T) {
	for _, change := range []string{"root", "session", "run", "role", "future_version", "native_target", "drop_wait"} {
		t.Run(change, func(t *testing.T) {
			f := newExecutionWaitFixture(t)
			if _, err := agents.NewController(f.s.db); err != nil {
				t.Fatal(err)
			}
			f.suspend(t, false)
			err := executionTx(context.Background(), f.s, func(tx *sql.Tx) error {
				row, err := readExecution(context.Background(), tx, f.req.Session.ID, f.req.RunID)
				if err != nil {
					return err
				}
				wait := &row.Manifest.TaskWaits[0]
				switch change {
				case "root":
					wait.Wait.RootBudgetID = "foreign"
				case "session":
					wait.Wait.SessionID = "foreign"
				case "run":
					wait.Wait.RunID = "foreign"
				case "role":
					wait.Wait.RoleID = "sibling"
				case "future_version":
					wait.Wait.AfterVersion = 100
				case "native_target":
					wait.NativeInterruptID = "forged"
				case "drop_wait":
					row.Manifest.TaskWaits = nil
				}
				return validateExecutionManifest(context.Background(), tx, row)
			})
			if !errors.Is(err, harness.ErrExecutionUnresumable) {
				t.Fatalf("accepted forged %s: %v", change, err)
			}
		})
	}
}

func TestExecutionTaskWaitAtomicPublicationClaimAndCancel(t *testing.T) {
	for _, operation := range []string{"publish", "claim", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			f := newExecutionWaitFixture(t)
			ctx := context.Background()
			var state harness.ExecutionState
			kind := "waiting_input"
			if operation != "publish" {
				state = f.suspend(t, false)
				f.complete(t)
				kind = "attempt_resuming"
				if operation == "cancel" {
					kind = "execution_cancelled"
				}
			}
			if _, err := f.s.db.Exec(`CREATE TRIGGER wait_fault BEFORE INSERT ON harness_execution_audits WHEN NEW.kind='` + kind + `' BEGIN SELECT RAISE(ABORT,'wait fault'); END`); err != nil {
				t.Fatal(err)
			}
			perform := func(tx *sql.Tx) error {
				switch operation {
				case "publish":
					if _, err := f.s.SuspendExecutionTx(ctx, tx, f.lease, f.checkpoint(t, false)); err != nil {
						return err
					}
					return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeWaitingInput)
				case "claim":
					lease, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
					if err != nil {
						return err
					}
					return f.ledger.BeginAttemptTx(ctx, tx, lease.Scope)
				default:
					_, err := f.s.CancelExecutionTx(ctx, tx, f.req.Session.ID, "owner", harness.CancelExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version})
					return err
				}
			}
			if err := executionTx(ctx, f.s, perform); err == nil {
				t.Fatal("fault did not roll back")
			}
			var count int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM eino_checkpoints`).Scan(&count); err != nil || count != map[bool]int{true: 0, false: 1}[operation == "publish"] {
				t.Fatalf("partial checkpoint: count=%d %v", count, err)
			}
			if _, err := f.s.db.Exec(`DROP TRIGGER wait_fault`); err != nil {
				t.Fatal(err)
			}
			mustExecutionTx(t, f.executionFixture, perform)
			if operation == "cancel" {
				err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
					_, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
					return err
				})
				if !errors.Is(err, harness.ErrExecutionConflict) {
					t.Fatalf("cancelled wait revived: %v", err)
				}
			}
		})
	}
}

func TestExecutionTaskWaitMixedPermissionCanResumeBeforeChild(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.pending(t)
	state := f.suspend(t, true)
	if !state.Resumable || len(state.WaitingInputs) != 1 || len(state.WaitingTasks) != 1 {
		t.Fatalf("mixed wait projection: %+v", state)
	}
	lease := f.resume(t, "owner")
	f.grant(t, lease)
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
		row, err := readExecution(context.Background(), tx, f.req.Session.ID, f.req.RunID)
		if err != nil {
			return err
		}
		targets, err := executionWaitTargets(context.Background(), tx, row)
		if err == nil && len(targets) != 0 {
			t.Fatal("unfinished child was resumed")
		}
		return err
	})
}

func TestExecutionTaskWaitRejectsUnauthorizedPublication(t *testing.T) {
	for _, change := range []string{"root", "session", "run", "role", "future_version"} {
		t.Run(change, func(t *testing.T) {
			f := newExecutionWaitFixture(t)
			if _, err := agents.NewController(f.s.db); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "root":
				f.wait.Wait.RootBudgetID = "foreign"
			case "session":
				f.wait.Wait.SessionID = "foreign"
			case "run":
				f.wait.Wait.RunID = "foreign"
			case "role":
				f.wait.Wait.RoleID = "sibling"
			case "future_version":
				f.wait.Wait.AfterVersion = 100
			}
			cp := f.checkpoint(t, false)
			err := executionTx(context.Background(), f.s, func(tx *sql.Tx) error {
				_, err := f.s.SuspendExecutionTx(context.Background(), tx, f.lease, cp)
				return err
			})
			if !errors.Is(err, harness.ErrPermissionDenied) {
				t.Fatalf("unauthorized publication %s: %v", change, err)
			}
		})
	}
}

func TestExecutionTaskWaitUncertainChildDoesNotEnableResume(t *testing.T) {
	for _, evidence := range []string{"blocked", "started", "uncertain"} {
		t.Run(evidence, func(t *testing.T) {
			f := newExecutionWaitFixture(t)
			state := f.suspend(t, false)
			f.complete(t)
			if evidence == "blocked" {
				if _, err := f.s.db.Exec(`UPDATE harness_background_bindings SET blocked_reason='external outcome uncertain' WHERE task_id=?`, f.child.binding.TaskID); err != nil {
					t.Fatal(err)
				}
			} else {
				// Fault injection: status and effect evidence disagree. The wait
				// gate must inspect receipts even if a terminal projection leaked.
				receipt := harness.ToolReceipt{RunID: f.child.binding.TaskID, SessionID: f.child.binding.ChildSessionID, ToolCallID: "unknown-effect", ToolName: "write_file", State: harness.ReceiptState(evidence), Version: 1}
				data, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.s.db.Exec(`INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,?,?,?,?,?)`, receipt.RunID, receipt.ToolCallID, receipt.SessionID, receipt.State, receipt.Version, data); err != nil {
					t.Fatal(err)
				}
			}
			current, err := f.s.Execution(context.Background(), f.req.Session.ID, f.req.RunID)
			if err != nil || current.Resumable || current.Status != harness.ExecutionWaitingInput {
				t.Fatalf("unsafe ready projection: %+v %v", current, err)
			}
			err = executionTx(context.Background(), f.s, func(tx *sql.Tx) error {
				_, err := f.s.ClaimExecutionResumeTx(context.Background(), tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
				return err
			})
			if !errors.Is(err, harness.ErrExecutionWaitingInput) {
				t.Fatalf("uncertain child resumed parent: %v", err)
			}
		})
	}
}
