package budget

import (
	"context"
	"database/sql"
	"errors"
)

// CancelPausedToolTx retires an undispatched admission from a prior waiting
// attempt. ToolCalls remain spent. The caller must verify the role stop and
// permission receipt atomically in this transaction before invoking it.
func (l *Ledger) CancelPausedToolTx(ctx context.Context, tx *sql.Tx, current Scope, operation string) error {
	old, _, err := readReservation(ctx, tx, operation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrScope
	}
	if err != nil {
		return err
	}
	if old.Scope.RootBudgetID != current.RootBudgetID || old.Scope.MemberID != current.MemberID || old.Scope.SessionID != current.SessionID || old.Scope.Fence >= current.Fence || old.Kind != "tool" || old.HeldTokens != 0 {
		return ErrScope
	}
	if old.State != "paused" {
		return ErrConflict
	}
	var state, outcome string
	if err = tx.QueryRowContext(ctx, `SELECT state,outcome FROM budget_attempts WHERE id=?`, old.Scope.AttemptID).Scan(&state, &outcome); err != nil {
		return err
	}
	if state != "ended" || outcome != string(OutcomeWaitingInput) {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE budget_reservations SET state='cancelled' WHERE id=? AND state='paused'`, operation)
	return err
}
