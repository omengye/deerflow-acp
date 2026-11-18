package deerflow

import (
	"context"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

// Optional extraction never hides cancellation, unknown spend, persistence or
// fencing failures in the main execution, even after an optional SQL rollback.
func memoryCriticalError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var persistence *budget.PersistenceError
	if errors.As(err, &persistence) {
		return err
	}
	for _, critical := range []error{context.Canceled, context.DeadlineExceeded, budget.ErrScope, budget.ErrConflict, budget.ErrUnknown, budget.ErrClock, harness.ErrExecutionConflict, harness.ErrExecutionUnresumable, harness.ErrTaskTreeStopped} {
		if errors.Is(err, critical) {
			return err
		}
	}
	return nil
}

func optionalMemoryAudit(ctx context.Context, store *memory.Store, runID, inputID, attemptID, status, sha string, notes []string) error {
	err := store.AuditExtraction(ctx, runID, inputID, attemptID, status, sha, notes)
	if err == nil {
		return nil
	}
	if critical := memoryCriticalError(ctx, err); critical != nil {
		return critical
	}
	var rolledBack *memory.OptionalWriteFailure
	if !errors.As(err, &rolledBack) {
		return err
	}
	store.AuditFallback(runID, inputID, "audit_unavailable")
	// One separate decision write, never a recursive audit.
	err = store.RecordDecision(ctx, runID, inputID, status)
	if critical := memoryCriticalError(ctx, err); critical != nil {
		return critical
	}
	if err != nil && !errors.As(err, &rolledBack) {
		return err
	}
	return nil
}

func optionalMemoryWriteError(ctx context.Context, store *memory.Store, runID, inputID, attemptID, status string, err error) error {
	if critical := memoryCriticalError(ctx, err); critical != nil {
		return critical
	}
	var rolledBack *memory.OptionalWriteFailure
	if !errors.As(err, &rolledBack) {
		return err
	}
	return optionalMemoryAudit(ctx, store, runID, inputID, attemptID, status, "", nil)
}
