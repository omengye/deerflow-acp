package eino

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// BudgetUsageOwnerTx executes inside the ledger's admission transaction. The
// role comes from the live engine attempt, never tool arguments or a native
// task ID. A continuation consequently retains its logical invocation owner.
func BudgetUsageOwnerTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) (budget.UsageOwner, error) {
	if work, ok := ctx.Value(roleWorkKey{}).(roleWork); ok && !work.controller.Durable() {
		return budget.UsageOwner{}, nil
	}
	role, ok, err := CurrentRoleTx(ctx, tx)
	if err != nil {
		return budget.UsageOwner{}, err
	}
	if !ok {
		return budget.UsageOwner{Known: true}, nil
	}
	if role.RootID != scope.RootBudgetID {
		return budget.UsageOwner{}, budget.ErrScope
	}
	return budget.UsageOwner{InvocationID: role.ID, Known: true}, nil
}
