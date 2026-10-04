package eino

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// These certificates concern only the model call and the task orchestration
// boundary. They never claim that earlier child tool calls had no effects.
type roleProviderError struct{ cause error }

func (e *roleProviderError) Error() string { return "subagent model request failed" }
func (e *roleProviderError) Unwrap() error { return e.cause }

type delegationFailure struct{ cause error }

func (e *delegationFailure) Error() string {
	var deadline *roleDeadline
	if errors.As(e.cause, &deadline) {
		return deadline.Error() + "; completed tool receipts remain authoritative, so do not repeat completed effects"
	}
	return "subagent execution failed; completed tool receipts remain authoritative, so do not repeat completed effects"
}
func (e *delegationFailure) Unwrap() error { return e.cause }

// Each attempt owns a distinct deadline cause. A parent/root cancellation
// cannot be mistaken for this role's configured time limit.
type roleDeadline struct{ name string }

func (e *roleDeadline) Error() string { return "subagent " + e.name + " exceeded its time limit" }

type roleEffectFailure struct{ cause error }

func (e *roleEffectFailure) Error() string {
	return "subagent tool effect is unconfirmed: " + e.cause.Error()
}
func (e *roleEffectFailure) Unwrap() error { return e.cause }

func certifiedToolOutcome(err error) bool {
	if err == nil || errors.Is(err, harness.ErrCommandUncertain) || isPermissionInterrupt(err) {
		return false
	}
	remaining, removed := removeErrorLeaves(err, func(leaf error) bool {
		switch leaf.(type) {
		case *harness.ToolNotExecutedError, *harness.ToolNoEffectError, *harness.ToolExecutionFailedError:
			return true
		}
		return false
	})
	return removed && remaining == nil
}

func recoverableRoleTimeout(err error) bool {
	var effect *roleEffectFailure
	var quota *budgetError
	var persistence interface{ PersistenceFailure() bool }
	if errors.As(err, &effect) || errors.As(err, &quota) || errors.As(err, &persistence) || errors.Is(err, harness.ErrCommandUncertain) || isPermissionInterrupt(err) {
		return false
	}
	remaining, _ := removeErrorLeaves(err, func(leaf error) bool {
		switch leaf.(type) {
		case *roleProviderError, *delegationFailure, *harness.ToolNotExecutedError, *harness.ToolNoEffectError, *adk.CancelError:
			return true
		}
		return leaf == context.Canceled || leaf == context.DeadlineExceeded || leaf == adk.ErrStreamCanceled || leaf == adk.ErrExceedMaxIterations
	})
	return remaining == nil
}

func (m *trackedModel) modelError(err error) error {
	if err == nil || !m.role {
		return err
	}
	var quota *budgetError
	var persistence interface{ PersistenceFailure() bool }
	if errors.As(err, &quota) || errors.As(err, &persistence) || errors.Is(err, harness.ErrCommandUncertain) || isPermissionInterrupt(err) {
		return err
	}
	return &roleProviderError{cause: err}
}

func recoverableRoleCause(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, harness.ErrCommandUncertain) || isPermissionInterrupt(err) {
		return false
	}
	var quota *budgetError
	var persistence interface{ PersistenceFailure() bool }
	var effect *roleEffectFailure
	if errors.As(err, &quota) || errors.As(err, &persistence) || errors.As(err, &effect) {
		return false
	}
	remaining, removed := removeErrorLeaves(err, func(leaf error) bool {
		switch leaf.(type) {
		case *roleProviderError, *delegationFailure, *harness.ToolNotExecutedError, *harness.ToolNoEffectError:
			return true
		}
		return leaf == adk.ErrExceedMaxIterations
	})
	return removed && remaining == nil
}

func (m *toolMiddleware) recoverableFailure(ctx context.Context, tc *adk.ToolContext, err error) bool {
	if recoverableToolFailure(ctx, err) {
		return true
	}
	if !m.nativeDelegation(tc) || err == nil || ctx.Err() != nil {
		return false
	}
	remaining, removed := removeErrorLeaves(err, func(leaf error) bool {
		_, ok := leaf.(*delegationFailure)
		return ok
	})
	return removed && remaining == nil
}
