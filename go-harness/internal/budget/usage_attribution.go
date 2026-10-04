package budget

import (
	"context"
	"database/sql"
	"errors"
)

// UsageOwner is a trusted host attribution, independent from quota authority.
// Empty InvocationID identifies lead work. Known=false leaves historical or
// external-engine work unattributed instead of guessing its logical role.
type UsageOwner struct {
	InvocationID string
	Known        bool
}

func migrateUsageAttribution(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS budget_operation_owners(
 operation_id TEXT PRIMARY KEY REFERENCES budget_reservations(id) ON DELETE CASCADE,
 root_id TEXT NOT NULL REFERENCES budget_roots(id),invocation_id TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS budget_operation_owner_role ON budget_operation_owners(root_id,invocation_id,operation_id);`)
	return err
}

func (l *Ledger) recordUsageOwnerTx(ctx context.Context, tx *sql.Tx, grant Reservation, parent string) error {
	if l.config.UsageOwnerTx == nil {
		return nil
	}
	owner, err := l.config.UsageOwnerTx(ctx, tx, grant.Scope)
	if err != nil {
		return err
	}
	if !owner.Known {
		return nil
	}
	if parent != "" {
		var root, invocation string
		err = tx.QueryRowContext(ctx, `SELECT root_id,invocation_id FROM budget_operation_owners WHERE operation_id=?`, parent).Scan(&root, &invocation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (root != grant.Scope.RootBudgetID || invocation != owner.InvocationID) {
			return ErrConflict
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO budget_operation_owners(operation_id,root_id,invocation_id) VALUES(?,?,?)`, grant.OperationID, grant.Scope.RootBudgetID, owner.InvocationID)
	return err
}
