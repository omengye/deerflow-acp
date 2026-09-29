package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

type runPersistenceError struct{ err error }

func (e *runPersistenceError) Error() string {
	return fmt.Sprintf("persist final run state: %v", e.err)
}
func (e *runPersistenceError) Unwrap() error            { return e.err }
func (e *runPersistenceError) PersistenceFailure() bool { return true }

func foregroundBudgetScope(req harness.RunRequest) budget.Scope {
	return budget.Scope{RootBudgetID: req.RootBudgetID, MemberID: req.RunID, SessionID: req.Session.ID, AttemptID: req.RunID, Fence: 1}
}

// keepBudgetAlive runs through execution and cleanup even after client cancel.
// A quota limit cancels further work but continues lease renewal until join.
// stop waits for the last SQL call, so it is safe to close the shared DB later.
func keepBudgetAlive(ctx context.Context, ledger *budget.Ledger, scope budget.Scope) (context.Context, func() error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var heartbeatErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(ledger.HeartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				persistCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), min(5*time.Second, ledger.HeartbeatInterval()))
				err := ledger.Heartbeat(persistCtx, scope)
				finish()
				if err == nil {
					continue
				}
				cancel(err)
				var limit *budget.LimitError
				if errors.As(err, &limit) {
					if heartbeatErr == nil {
						heartbeatErr = err
					}
					continue
				}
				heartbeatErr = errors.Join(heartbeatErr, err)
				return
			}
		}
	}()
	return runCtx, func() error {
		once.Do(func() { close(stop) })
		<-done
		cancel(nil)
		return heartbeatErr
	}
}

func applyBudgetHeartbeat(result *harness.RunResult, err error) error {
	if err == nil {
		return nil
	}
	// Only a standalone quota stop is normal. Joined persistence failures must
	// retain the full failure even when a limit was reported earlier.
	limit, ok := err.(*budget.LimitError)
	if !ok {
		return err
	}
	result.Limit = limit.Resource
	switch limit.Resource {
	case "tokens":
		result.StopReason = "max_tokens"
	case "time":
		result.StopReason = "cancelled"
	default:
		result.StopReason = "max_turn_requests"
	}
	return nil
}
