package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type automaticStartedKey struct{}

// The optional observer belongs to the live automatic subscription, never to a
// previously completed prompt. Read under the lease, notify outside the tx.
func (s *Service) notifyAutomaticExecutionStarted(ctx context.Context, lease ExecutionLease) error {
	started, _ := ctx.Value(automaticStartedKey{}).(func(context.Context, harness.ExecutionState) error)
	if started == nil {
		return nil
	}
	var state harness.ExecutionState
	if err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		row, err := checkExecutionLease(ctx, tx, lease)
		if err != nil {
			return err
		}
		state = row.State
		return nil
	}); err != nil {
		return err
	}
	return started(ctx, state)
}
