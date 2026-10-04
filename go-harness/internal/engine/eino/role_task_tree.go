package eino

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type roleTreeObservationKey struct{}
type roleTreeObservation struct {
	mu     sync.Mutex
	result *string
}

func (r *timedRole) stoppedRoleResult(ctx context.Context, id string) error {
	if hooks := executionHooks(ctx); hooks != nil {
		broker, ok := hooks.Broker.(interaction.RoleCancellationBroker)
		if !ok {
			return errors.New("stopped role requires host checkpoint cancellation support")
		}
		if err := broker.CancelStoppedRole(ctx, id); err != nil {
			return err
		}
	}
	if err := r.controller.RecordStopped(ctx, r.rootID, id); err != nil {
		return err
	}
	return &delegationFailure{cause: agents.ErrStopped}
}

func withoutRoleStop(err error) error {
	result, _ := removeErrorLeaves(err, func(leaf error) bool { return leaf == agents.ErrStopped })
	return result
}

func observeRoleTreeResult(ctx context.Context, messages []*schema.Message) {
	o, _ := ctx.Value(roleTreeObservationKey{}).(*roleTreeObservation)
	if o == nil || len(messages) == 0 {
		return
	}
	last := messages[len(messages)-1]
	if last == nil || last.Role != schema.Assistant || len(last.ToolCalls) != 0 {
		return
	}
	text := last.Content
	o.mu.Lock()
	o.result = &text
	o.mu.Unlock()
}
func (o *roleTreeObservation) final() *string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.result == nil {
		return nil
	}
	copy := *o.result
	return &copy
}

// CheckTaskTreeEffectTx runs in the ledger's existing admission transaction.
// It fences both root calls and role calls without granting any new authority.
func CheckTaskTreeEffectTx(ctx context.Context, tx *sql.Tx, rootID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_task_tree_stops')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	role := ""
	if work, ok := ctx.Value(roleWorkKey{}).(roleWork); ok {
		if work.rootID != rootID {
			return agents.ErrInvocationConflict
		}
		role = work.id
	}
	return agents.CheckStopped(ctx, tx, rootID, role)
}
