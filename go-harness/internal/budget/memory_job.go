package budget

import (
	"context"
	"database/sql"
	"errors"
)

// FinishMemoryAttemptTx reconciles exactly one host-owned memory call before
// general restart reconciliation runs. The host must fence its job in this tx.
// A durable response may settle an expired attempt; it never authorizes new I/O.
// Absent response evidence, a dispatched call remains unknown and keeps its hold.
func (l *Ledger) FinishMemoryAttemptTx(ctx context.Context, tx *sql.Tx, scope Scope, operation string, response *Settlement) (unknown bool, err error) {
	defer func() { err = persist("finish memory attempt", err) }()
	var kind string
	if err = tx.QueryRowContext(ctx, "SELECT kind FROM budget_members WHERE id=? AND root_id=? AND session_id=?", scope.MemberID, scope.RootBudgetID, scope.SessionID).Scan(&kind); err != nil {
		return false, err
	}
	if kind != "memory_job" {
		return false, ErrScope
	}
	if err = checkOwner(ctx, tx, scope, false); err != nil {
		return false, err
	}
	// Memory attempts own at most one model reservation; all retries get a new
	// fence. Refuse to clean up an unexpected operation or a tool reservation.
	rows, e := tx.QueryContext(ctx, "SELECT id FROM budget_reservations WHERE attempt_id=?", scope.AttemptID)
	if e != nil {
		return false, e
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return false, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	if len(ids) > 1 || len(ids) == 1 && ids[0] != operation {
		return false, ErrConflict
	}
	r, e := readRoot(ctx, tx, scope.RootBudgetID)
	if e != nil {
		return false, e
	}
	if e = l.advance(ctx, tx, r, l.config.Now().UnixNano()); e != nil {
		return false, e
	}
	if e = saveRoot(ctx, tx, r); e != nil {
		return false, e
	}
	if len(ids) == 1 {
		grant, _, e := readReservation(ctx, tx, operation)
		if e != nil {
			return false, e
		}
		if grant.Scope != scope || grant.Kind != "model" {
			return false, ErrScope
		}
		switch grant.State {
		case "reserved":
			// Check persisted dispatch state before advance can expire the attempt.
			return false, ErrConflict
		case "cancelled":
		case "settled":
			if response != nil {
				if _, e = l.SettleTx(ctx, tx, grant, *response); e != nil {
					return false, e
				}
			}
		case "dispatched", "unknown":
			if response == nil {
				unknown = true
				response = &Settlement{Unknown: true}
			}
			if _, e = l.SettleTx(ctx, tx, grant, *response); e != nil {
				return false, e
			}
			unknown = response.Unknown
		default:
			return false, ErrConflict
		}
	} else if response != nil {
		return false, ErrConflict
	}
	state, outcome := "ended", "completed"
	if unknown {
		state, outcome = "unknown", "interrupted"
	}
	if _, e = tx.ExecContext(ctx, "UPDATE budget_attempts SET state=?,outcome=? WHERE id=?", state, outcome, scope.AttemptID); e != nil {
		return false, e
	}
	// Do not clear another member's unresolved work or any spent quota.
	if !unknown {
		r, e = readRoot(ctx, tx, scope.RootBudgetID)
		if e != nil {
			return false, e
		}
		var unresolved bool
		e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM budget_attempts WHERE root_id=? AND state='unknown') OR EXISTS(SELECT 1 FROM budget_reservations WHERE root_id=? AND state='unknown')", scope.RootBudgetID, scope.RootBudgetID).Scan(&unresolved)
		if e != nil {
			return false, e
		}
		if !unresolved && r.BlockedReason == "unknown" {
			r.BlockedReason = ""
			if r.Limits.MaxTokens > 0 && r.SpentTokens >= r.Limits.MaxTokens {
				r.BlockedReason = "tokens"
			}
			if r.Limits.Timeout > 0 && r.Elapsed >= r.Limits.Timeout {
				r.BlockedReason = "time"
			}
			if e = saveRoot(ctx, tx, r); e != nil {
				return false, e
			}
		}
	}
	return unknown, nil
}

// ReleaseMemoryReservationTx releases only an admission proved never dispatched.
// Calls remain counted conservatively; token holds are released exactly once.
func (l *Ledger) ReleaseMemoryReservationTx(ctx context.Context, tx *sql.Tx, scope Scope, operation string) (err error) {
	defer func() { err = persist("release memory reservation", err) }()
	grant, _, err := readReservation(ctx, tx, operation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if grant.Scope != scope || grant.Kind != "model" {
		return ErrScope
	}
	var kind string
	if err = tx.QueryRowContext(ctx, "SELECT kind FROM budget_members WHERE id=?", scope.MemberID).Scan(&kind); err != nil {
		return err
	}
	if kind != "memory_job" {
		return ErrScope
	}
	if grant.State == "cancelled" {
		return nil
	}
	if grant.State != "reserved" {
		return ErrConflict
	}
	r, err := readRoot(ctx, tx, scope.RootBudgetID)
	if err != nil {
		return err
	}
	if r.HeldTokens < grant.HeldTokens {
		return ErrConflict
	}
	r.HeldTokens -= grant.HeldTokens
	if _, err = tx.ExecContext(ctx, "UPDATE budget_reservations SET state='cancelled' WHERE id=?", operation); err != nil {
		return err
	}
	return saveRoot(ctx, tx, r)
}
