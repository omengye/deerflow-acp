package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Explicit deletion may remove an old fault that required the retired manual
// reconciliation flow. It does not migrate that fault or assert an outcome.
// Both durable records must agree, and every existing activity/accounting
// guard still applies under the coordinator fence and SQLite writer lock.
// Automatic archival deliberately keeps checkArchiveExecutionTx's allowlist.
func checkDeleteExecutionTx(ctx context.Context, tx *sql.Tx, id string, tables map[string]bool) error {
	for _, check := range []struct{ table, query string }{
		{"harness_runs", `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND status NOT IN ('completed','failed','cancelled','needs_reconciliation'))`},
		{"harness_executions", `SELECT EXISTS(SELECT 1 FROM harness_executions WHERE session_id=? AND status NOT IN ('completed','failed','cancelled','needs_reconciliation'))`},
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
		// A missing execution record cannot prove an old fault is quiescent.
		var legacy bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND status='needs_reconciliation')`, id).Scan(&legacy); err != nil {
			return err
		}
		if legacy {
			return harness.ErrBusy
		}
		return nil
	}
	var mismatched bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM harness_runs r WHERE r.session_id=? AND r.status='needs_reconciliation' AND NOT EXISTS(SELECT 1 FROM harness_executions e WHERE e.run_id=r.id AND e.session_id=r.session_id AND e.input_id=r.input_id AND e.status=r.status)) OR
 EXISTS(SELECT 1 FROM harness_executions e WHERE e.session_id=? AND e.status='needs_reconciliation' AND NOT EXISTS(SELECT 1 FROM harness_runs r WHERE r.id=e.run_id AND r.session_id=e.session_id AND r.input_id=e.input_id AND r.status=e.status))`, id, id).Scan(&mismatched); err != nil {
		return err
	}
	if mismatched {
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
