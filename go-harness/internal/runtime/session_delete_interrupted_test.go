package runtime

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func interruptedDeletionFixture(t *testing.T) (*executionFixture, *Service) {
	t.Helper()
	ctx := context.Background()
	f := newExecutionFixture(t)
	reservation, err := f.ledger.ReserveModel(ctx, f.lease.Scope, budget.ModelRequest{
		OperationID: NewID(), Digest: "interrupted-model", InputTokens: 100, MaxOutputTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.ledger.MarkDispatched(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	// Joined I/O without final usage conservatively charges the reservation.
	if _, err = f.ledger.Settle(ctx, reservation, budget.Settlement{Complete: false}); err != nil {
		t.Fatal(err)
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		state, err := f.s.EndExecutionAttemptTx(ctx, tx, f.lease, harness.ExecutionStopDisconnected, "connection closed")
		if err != nil {
			return err
		}
		if state.Status != harness.ExecutionFailed {
			t.Fatalf("interrupted execution is not terminal: %+v", state)
		}
		return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeInterrupted)
	})
	return f, NewService(f.s, nil, "")
}

func TestExplicitDeleteSettledInterruptedAttempt(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "detached", true: "attached"}[attached], func(t *testing.T) {
			ctx := context.Background()
			f, service := interruptedDeletionFixture(t)
			id := f.req.Session.ID
			other, err := f.s.CreateSession(ctx, t.TempDir(), "unrelated")
			if err != nil {
				t.Fatal(err)
			}
			// Automatic archival remains conservative and never alters the ledger.
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			tables, err := sessionTables(ctx, tx)
			if err == nil {
				err = checkArchiveExecutionTx(ctx, tx, id, tables)
			}
			tx.Rollback()
			if !errors.Is(err, harness.ErrBusy) {
				t.Fatalf("archive accepted interrupted accounting: %v", err)
			}
			snapshot, err := f.ledger.Snapshot(ctx, f.req.RootBudgetID)
			if err != nil || snapshot.HeldTokens != 0 || snapshot.SpentTokens != 200 || snapshot.BlockedReason != "unknown" {
				t.Fatalf("unexpected interrupted ledger: %+v %v", snapshot, err)
			}
			var already bool
			if attached {
				if _, err = service.Coordinator.Attach(id, "owner"); err != nil {
					t.Fatal(err)
				}
				already, err = service.DeleteAttachedSession(ctx, "owner", id)
			} else {
				already, err = service.DeleteSession(ctx, id)
			}
			if err != nil || already {
				t.Fatalf("explicit deletion of settled interrupted history: %v %v", already, err)
			}
			for _, table := range []string{"harness_runs", "harness_executions", "harness_execution_attempts", "budget_roots", "budget_attempts", "budget_reservations"} {
				var count int
				if err = f.s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s retained private state: %d %v", table, count, err)
				}
			}
			if _, err = f.s.Session(ctx, other.ID); err != nil {
				t.Fatalf("unrelated session removed: %v", err)
			}
			if already, err = service.DeleteSession(ctx, id); err != nil || !already {
				t.Fatalf("retry is not idempotent: %v %v", already, err)
			}
		})
	}
}

func TestExplicitDeleteInterruptedAttemptRequiresTerminalEvidence(t *testing.T) {
	for name, query := range map[string]string{
		"active_budget":      `UPDATE budget_attempts SET state='active'`,
		"held_tokens":        `UPDATE budget_roots SET held_tokens=1`,
		"reserved":           `UPDATE budget_reservations SET state='reserved'`,
		"dispatched":         `UPDATE budget_reservations SET state='dispatched'`,
		"unknown_operation":  `UPDATE budget_reservations SET state='unknown'`,
		"missing_attempt":    `DELETE FROM harness_execution_attempts`,
		"unfinished_attempt": `UPDATE harness_execution_attempts SET finished_at=''`,
		"waiting_attempt":    `UPDATE harness_execution_attempts SET status='waiting_input'`,
		"future_attempt":     `UPDATE harness_execution_attempts SET status='future_state'`,
		"wrong_fence":        `UPDATE budget_attempts SET fence=2`,
		"wrong_session":      `UPDATE budget_attempts SET session_id='another-session'`,
		"wrong_execution":    `UPDATE harness_executions SET status='completed'`,
	} {
		t.Run(name, func(t *testing.T) {
			f, service := interruptedDeletionFixture(t)
			if _, err := f.s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := service.DeleteSession(context.Background(), f.req.Session.ID); !errors.Is(err, harness.ErrBusy) {
				t.Fatalf("%s did not block deletion: %v", name, err)
			}
			if _, err := f.s.Session(context.Background(), f.req.Session.ID); err != nil {
				t.Fatalf("blocked deletion lost history: %v", err)
			}
		})
	}
}
