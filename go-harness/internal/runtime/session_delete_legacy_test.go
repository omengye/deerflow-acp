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

func legacyDeletionFixture(t *testing.T) (*executionFixture, *Service, harness.ExecutionState) {
	t.Helper()
	f, service, state := legacyFaultFixture(t)
	// The reported history has only completed/failed/no-effect receipts; its
	// legacy manual-review status is the sole blocker, not uncertain effects.
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		r, err := readReceipt(context.Background(), tx, f.req.Session.ID, f.req.RunID, "write")
		if err != nil {
			return err
		}
		r.State = harness.ReceiptFailed
		return writeReceipt(context.Background(), tx, r)
	})
	return f, service, state
}

func appendDeletionHistory(t *testing.T, f *executionFixture, status string) harness.RunRequest {
	t.Helper()
	ctx := context.Background()
	req := f.req
	req.RunID, req.InputID = NewID(), NewID()
	req.RootBudgetID = req.RunID
	if err := f.s.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	scope := budget.Scope{RootBudgetID: req.RunID, MemberID: req.RunID, SessionID: req.Session.ID, AttemptID: NewID(), Fence: 1}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		if err := f.ledger.CreateRootTx(ctx, tx, budget.RootSpec{RootBudgetID: req.RunID, SessionID: req.Session.ID, RootRunID: req.RunID}); err != nil {
			return err
		}
		if err := f.ledger.BeginAttemptTx(ctx, tx, scope); err != nil {
			return err
		}
		lease, err := f.s.BeginExecutionTx(ctx, tx, req, "history-owner", scope)
		if err != nil {
			return err
		}
		if _, err = f.s.EndExecutionAttemptTx(ctx, tx, lease, harness.ExecutionStopCancelled, "ended historical turn"); err != nil {
			return err
		}
		if err = f.ledger.EndAttemptTx(ctx, tx, scope, budget.OutcomeCancelled); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE harness_runs SET status=?,stop_reason='end_turn' WHERE id=?`, status, req.RunID); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE harness_executions SET status=? WHERE run_id=?`, status, req.RunID)
		return err
	})
	return req
}

func TestExplicitDeleteQuiescentLegacyFaultHistory(t *testing.T) {
	ctx := context.Background()
	f, service, state := legacyDeletionFixture(t)
	appendDeletionHistory(t, f, "needs_reconciliation")
	appendDeletionHistory(t, f, "needs_reconciliation")
	appendDeletionHistory(t, f, "cancelled")
	other, err := f.s.CreateSession(ctx, t.TempDir(), "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -31).Format(time.RFC3339Nano)
	if _, err = f.s.db.Exec(`UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, f.req.Session.ID); err != nil {
		t.Fatal(err)
	}
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 5, InactiveDays: 5, CheckInterval: time.Hour}
	if archived, _, err := f.s.archiveSession(ctx, f.req.Session.ID, time.Now().UTC(), policy); archived || !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("automatic archive changed legacy history: %v %v", archived, err)
	}
	unchanged, err := f.s.Execution(ctx, f.req.Session.ID, state.RunID)
	if err != nil || unchanged.Status != harness.ExecutionNeedsReconciliation || unchanged.Version != state.Version {
		t.Fatalf("eligibility query migrated old fault: %+v %v", unchanged, err)
	}
	if _, err = service.DeleteSession(ctx, f.req.Session.ID); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("management deleted another connection's attachment: %v", err)
	}
	if already, err := service.DeleteAttachedSession(ctx, "maintenance", f.req.Session.ID); err != nil || already {
		t.Fatalf("explicit owner deletion of legacy history: %v %v", already, err)
	}
	for _, table := range []string{"harness_runs", "harness_executions", "harness_execution_attempts", "harness_tool_receipts", "budget_roots", "budget_attempts", "eino_checkpoints"} {
		var count int
		if err = f.s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained deleted history: %d %v", table, count, err)
		}
	}
	if _, err = f.s.Session(ctx, other.ID); err != nil {
		t.Fatalf("unrelated session removed: %v", err)
	}
	if already, err := service.DeleteSession(ctx, f.req.Session.ID); err != nil || !already {
		t.Fatalf("deletion retry not idempotent: %v %v", already, err)
	}
}

func TestExplicitDeleteLegacyFaultKeepsActivityAndEvidenceGuards(t *testing.T) {
	for _, blocker := range []string{"run_running", "run_pending", "run_future", "execution_running", "execution_future", "run_mismatch", "execution_mismatch", "input_mismatch", "execution_attempt", "budget_active", "budget_unknown", "reserved", "dispatched", "unknown", "pending_receipt", "started_receipt", "uncertain_receipt"} {
		t.Run(blocker, func(t *testing.T) {
			ctx := context.Background()
			f, _, state := legacyDeletionFixture(t)
			expected := error(harness.ErrBusy)
			var err error
			switch blocker {
			case "run_running", "run_pending", "run_future", "run_mismatch":
				value := map[string]string{"run_running": "running", "run_pending": "pending", "run_future": "future_state", "run_mismatch": "completed"}[blocker]
				_, err = f.s.db.Exec(`UPDATE harness_runs SET status=? WHERE id=?`, value, state.RunID)
			case "execution_running", "execution_future", "execution_mismatch":
				value := map[string]string{"execution_running": "running", "execution_future": "future_state", "execution_mismatch": "completed"}[blocker]
				_, err = f.s.db.Exec(`UPDATE harness_executions SET status=? WHERE run_id=?`, value, state.RunID)
			case "input_mismatch":
				other := appendDeletionHistory(t, f, "cancelled")
				_, err = f.s.db.Exec(`UPDATE harness_executions SET input_id=? WHERE run_id=?`, other.InputID, state.RunID)
			case "execution_attempt":
				_, err = f.s.db.Exec(`UPDATE harness_execution_attempts SET status='running' WHERE id=?`, state.AttemptID)
			case "budget_active", "budget_unknown":
				value := map[string]string{"budget_active": "active", "budget_unknown": "unknown"}[blocker]
				_, err = f.s.db.Exec(`UPDATE budget_attempts SET state=? WHERE id=?`, value, state.AttemptID)
			case "reserved", "dispatched", "unknown":
				_, err = f.s.db.Exec(`INSERT INTO budget_reservations(id,root_id,member_id,session_id,attempt_id,fence,kind,digest,input_tokens,output_cap,held_tokens,state) VALUES('unsettled',?,?,?,?,1,'model','test',0,1,1,?)`, f.req.RunID, f.req.RunID, f.req.Session.ID, state.AttemptID, blocker)
			default:
				value := map[string]string{"pending_receipt": "pending", "started_receipt": "started", "uncertain_receipt": "uncertain"}[blocker]
				_, err = f.s.db.Exec(`UPDATE harness_tool_receipts SET state=? WHERE run_id=?`, value, state.RunID)
				expected = harness.ErrReceiptConflict
			}
			if err != nil {
				t.Fatal(err)
			}
			var before []byte
			if err = f.s.db.QueryRow(`SELECT receipt FROM harness_tool_receipts WHERE run_id=?`, state.RunID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, _, err = f.s.purgeSession(ctx, f.req.Session.ID, false); !errors.Is(err, expected) {
				t.Fatalf("%s guard was waived: %v", blocker, err)
			}
			var after []byte
			if err = f.s.db.QueryRow(`SELECT receipt FROM harness_tool_receipts WHERE run_id=?`, state.RunID).Scan(&after); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("blocked deletion changed evidence: %v", err)
			}
			if _, err = f.s.Session(ctx, f.req.Session.ID); err != nil {
				t.Fatalf("blocked deletion lost the session: %v", err)
			}
		})
	}
}
