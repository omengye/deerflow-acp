package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Explicit deletion can discard a quiescent legacy fault or a saved wait
// whose automatic recovery is exhausted. Both durable records must agree.
// Archival uses its own terminal allowlist.
func checkDeleteExecutionTx(ctx context.Context, tx *sql.Tx, id string, tables map[string]bool) error {
	for _, check := range []struct{ table, query string }{
		{"harness_runs", `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND status NOT IN ('completed','failed','cancelled','needs_reconciliation','waiting_input'))`},
		{"harness_executions", `SELECT EXISTS(SELECT 1 FROM harness_executions WHERE session_id=? AND status NOT IN ('completed','failed','cancelled','needs_reconciliation','waiting_input'))`},
		{"harness_execution_attempts", `SELECT EXISTS(SELECT 1 FROM harness_execution_attempts a JOIN harness_runs r ON r.id=a.run_id WHERE r.session_id=? AND a.status='running')`},
		{"budget_attempts", `SELECT EXISTS(SELECT 1 FROM budget_attempts a JOIN budget_roots r ON r.id=a.root_id WHERE r.session_id=? AND a.state='active')`},
		{"budget_reservations", `SELECT EXISTS(SELECT 1 FROM budget_reservations v JOIN budget_roots r ON r.id=v.root_id WHERE r.session_id=? AND v.state IN ('reserved','dispatched','unknown'))`},
		{"budget_roots", `SELECT EXISTS(SELECT 1 FROM budget_roots WHERE session_id=? AND held_tokens<>0)`},
	} {
		if !tables[check.table] {
			continue
		}
		var blocked bool
		if err := tx.QueryRowContext(ctx, check.query, id).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return harness.ErrBusy
		}
	}
	if err := checkDeleteInterruptedAttemptsTx(ctx, tx, id, tables); err != nil {
		return err
	}
	if !tables["harness_executions"] {
		var needsExecution bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND status IN ('needs_reconciliation','waiting_input'))`, id).Scan(&needsExecution); err != nil {
			return err
		}
		if needsExecution {
			return harness.ErrBusy
		}
		return nil
	}
	var mismatched bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM harness_runs r WHERE r.session_id=? AND r.status IN ('needs_reconciliation','waiting_input') AND NOT EXISTS(SELECT 1 FROM harness_executions e WHERE e.run_id=r.id AND e.session_id=r.session_id AND e.input_id=r.input_id AND e.status=r.status)) OR
 EXISTS(SELECT 1 FROM harness_executions e WHERE e.session_id=? AND e.status IN ('needs_reconciliation','waiting_input') AND NOT EXISTS(SELECT 1 FROM harness_runs r WHERE r.id=e.run_id AND r.session_id=e.session_id AND r.input_id=e.input_id AND r.status=e.status))`, id, id).Scan(&mismatched); err != nil {
		return err
	}
	if mismatched {
		return harness.ErrBusy
	}
	return checkDeleteWaitingExecutionTx(ctx, tx, id, tables)
}

func checkDeleteWaitingExecutionTx(ctx context.Context, tx *sql.Tx, id string, tables map[string]bool) error {
	// A retry counter belongs to one checkpoint. The last claim may still be
	// running, so require its matching execution and ledger attempts to have
	// ended as well. Missing evidence leaves a waiting session protected.
	query := `SELECT EXISTS(SELECT 1 FROM harness_executions e WHERE e.session_id=? AND e.status='waiting_input'`
	args := []any{id}
	if tables["harness_execution_wakeups"] && tables["harness_execution_attempts"] && tables["budget_attempts"] {
		query += ` AND NOT EXISTS (
 SELECT 1 FROM harness_execution_wakeups w
 JOIN harness_execution_attempts x ON x.run_id=w.run_id AND x.id=e.attempt_id AND x.ordinal=e.attempt
 JOIN budget_attempts b ON b.id=x.id AND b.member_id=e.run_id AND b.session_id=e.session_id AND b.root_id=e.root_budget_id AND b.fence=x.ordinal
 WHERE w.run_id=e.run_id AND w.attempts>=? AND w.checkpoint_sha<>''
 AND w.checkpoint_sha=CASE WHEN json_valid(CAST(e.manifest AS TEXT)) THEN json_extract(CAST(e.manifest AS TEXT),'$.CheckpointSHA') END
 AND x.finished_at<>'' AND x.status IN ('waiting_input','failed','disconnected','interrupted') AND b.state='ended')`
		args = append(args, automaticExecutionAttemptLimit)
	}
	var busy bool
	if err := tx.QueryRowContext(ctx, query+`)`, args...).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return harness.ErrBusy
	}
	return nil
}

func checkDeleteInterruptedAttemptsTx(ctx context.Context, tx *sql.Tx, id string, tables map[string]bool) error {
	if !tables["budget_attempts"] {
		return nil
	}
	// EndAttemptTx records every interrupted attempt as unknown, including
	// joined calls whose reservations are already fully charged. Explicit
	// deletion may discard that historical marker only when the same fenced
	// execution attempt durably ended and its run is terminal. No ledger state
	// or tool outcome is rewritten; archival retains its stricter guard.
	query := `SELECT EXISTS(SELECT 1 FROM budget_attempts a JOIN budget_roots root ON root.id=a.root_id WHERE root.session_id=? AND a.state='unknown'`
	if tables["harness_execution_attempts"] && tables["harness_executions"] && tables["harness_runs"] {
		query += ` AND NOT EXISTS(
 SELECT 1 FROM harness_execution_attempts x
 JOIN harness_executions e ON e.run_id=x.run_id
 JOIN harness_runs r ON r.id=e.run_id
 WHERE x.id=a.id AND x.run_id=a.member_id AND x.ordinal=a.fence
 AND x.status IN ('completed','failed','cancelled','disconnected') AND x.finished_at<>''
 AND e.session_id=a.session_id AND e.root_budget_id=a.root_id
 AND r.session_id=e.session_id AND r.input_id=e.input_id AND r.status=e.status
 AND r.status IN ('completed','failed','cancelled'))`
	}
	query += `)`
	var blocked bool
	if err := tx.QueryRowContext(ctx, query, id).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return harness.ErrBusy
	}
	return nil
}
