package budget

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

func migrateToolContinuations(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS budget_tool_continuations (
 parent_id TEXT PRIMARY KEY REFERENCES budget_reservations(id),
 child_id TEXT UNIQUE NOT NULL REFERENCES budget_reservations(id),
 CHECK(parent_id<>child_id)
)`)
	return persist("migrate tool continuations", err)
}

// PauseTool saves an undispatched protected tool's counted admission for a
// later native HITL attempt. Pausing does not refund or increment ToolCalls.
// A paused operation cannot itself authorize I/O; ResumeTool mints the next
// attempt-owned reservation after the predecessor has ended waiting_input.
func (l *Ledger) PauseTool(ctx context.Context, grant Reservation) error {
	return l.transaction(ctx, "pause tool", func(tx *sql.Tx) error {
		old, _, err := readReservation(ctx, tx, grant.OperationID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrScope
		}
		if err != nil {
			return err
		}
		if old.Scope != grant.Scope || old.Kind != "tool" || old.HeldTokens != 0 {
			return ErrScope
		}
		if old.State != "reserved" && old.State != "paused" {
			return ErrConflict
		}
		r, err := readRoot(ctx, tx, old.Scope.RootBudgetID)
		if err != nil {
			return err
		}
		if err = l.advance(ctx, tx, r, l.config.Now().UnixNano()); err != nil {
			return err
		}
		if err = l.checkEffect(ctx, tx, old.Scope); err != nil {
			return err
		}
		if old.State == "paused" {
			return saveRoot(ctx, tx, r)
		}
		result, err := tx.ExecContext(ctx, "UPDATE budget_reservations SET state='paused' WHERE id=? AND state='reserved'", old.OperationID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrConflict
		}
		return saveRoot(ctx, tx, r)
	})
}

// ResumeTool transfers one counted tool admission to a new fenced attempt.
// The predecessor is consumed once. A repeat of the exact same continuation
// returns its stored grant; only MarkDispatched's first=true authorizes I/O.
func (l *Ledger) ResumeTool(ctx context.Context, scope Scope, request ToolRequest, pausedOperationID string) (grant Reservation, err error) {
	if !validScope(scope) || request.OperationID == "" || request.Digest == "" || pausedOperationID == "" || request.OperationID == pausedOperationID {
		return grant, ErrScope
	}
	// ReserveTool binds the caller digest together with its zero token fields.
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/0/0", request.Digest))))
	err = l.transaction(ctx, "resume tool", func(tx *sql.Tx) error {
		existing, existingDigest, e := readReservation(ctx, tx, request.OperationID)
		if e == nil {
			if existing.Scope != scope || existing.Kind != "tool" || existingDigest != digest {
				return ErrConflict
			}
			var parent string
			if e = tx.QueryRowContext(ctx, "SELECT parent_id FROM budget_tool_continuations WHERE child_id=?", request.OperationID).Scan(&parent); errors.Is(e, sql.ErrNoRows) {
				return ErrConflict
			} else if e != nil {
				return e
			}
			if parent != pausedOperationID {
				return ErrConflict
			}
			grant = existing
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		parent, parentDigest, e := readReservation(ctx, tx, pausedOperationID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrScope
		}
		if e != nil {
			return e
		}
		if parent.Kind != "tool" || parent.HeldTokens != 0 || parent.Scope.RootBudgetID != scope.RootBudgetID || parent.Scope.MemberID != scope.MemberID || parent.Scope.SessionID != scope.SessionID {
			return ErrScope
		}
		if parent.State != "paused" || parentDigest != digest || parent.Scope.AttemptID == scope.AttemptID || parent.Scope.Fence >= scope.Fence {
			return ErrConflict
		}
		if e = checkOwner(ctx, tx, parent.Scope, false); e != nil {
			return e
		}
		var state, outcome string
		if e = tx.QueryRowContext(ctx, "SELECT state,outcome FROM budget_attempts WHERE id=?", parent.Scope.AttemptID).Scan(&state, &outcome); e != nil {
			return e
		}
		if state != "ended" || outcome != string(OutcomeWaitingInput) {
			return ErrScope
		}
		r, e := readRoot(ctx, tx, scope.RootBudgetID)
		if e != nil {
			return e
		}
		if e = l.advance(ctx, tx, r, l.config.Now().UnixNano()); e != nil {
			return e
		}
		if e = blocked(r); e != nil {
			return e
		}
		if e = l.checkEffect(ctx, tx, scope); e != nil {
			return e
		}
		grant = Reservation{OperationID: request.OperationID, Scope: scope, Kind: "tool", State: "reserved"}
		if _, e = tx.ExecContext(ctx, `INSERT INTO budget_reservations(id,root_id,member_id,session_id,attempt_id,fence,kind,digest,input_tokens,output_cap,held_tokens,state) VALUES(?,?,?,?,?,?,'tool',?,0,0,0,'reserved')`, grant.OperationID, scope.RootBudgetID, scope.MemberID, scope.SessionID, scope.AttemptID, scope.Fence, digest); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO budget_tool_continuations(parent_id,child_id) VALUES(?,?)", pausedOperationID, request.OperationID); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, "UPDATE budget_reservations SET state='continued' WHERE id=? AND state='paused'", pausedOperationID)
		if e != nil {
			return e
		}
		changed, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if changed != 1 {
			return ErrConflict
		}
		// Time/revision advance is durable, but this logical tool was already
		// counted by its first reservation. Never increment ToolCalls here.
		return saveRoot(ctx, tx, r)
	})
	return
}
