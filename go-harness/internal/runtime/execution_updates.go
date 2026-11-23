package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type executionStartedKey struct{}

// WithExecutionStarted scopes a committed execution's identity to the request
// or automatic subscription which owns it.
func WithExecutionStarted(ctx context.Context, started func(context.Context, harness.ExecutionState) error) context.Context {
	return context.WithValue(ctx, executionStartedKey{}, started)
}

func (s *Service) notifyExecutionStarted(ctx context.Context, lease ExecutionLease) error {
	started, _ := ctx.Value(executionStartedKey{}).(func(context.Context, harness.ExecutionState) error)
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
