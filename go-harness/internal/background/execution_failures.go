package background

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Native Manager treats ErrDrainCheckpointUnavailable as an instruction to
// leave the attempt running. After all I/O has joined, that would leak the
// budget attempt instead of durably recording a failure. Suppress only that
// sentinel match; Is/As retain every underlying persistence/provider error.
type joinedDrainFailure struct{ cause error }

func (e *joinedDrainFailure) Error() string { return e.cause.Error() }
func (e *joinedDrainFailure) Is(target error) bool {
	return target != bt.ErrDrainCheckpointUnavailable && errors.Is(e.cause, target)
}
func (e *joinedDrainFailure) As(target any) bool { return errors.As(e.cause, target) }

// Ordinary cancellation is an outcome, not an execution failure. Persistence
// wrappers are atomic: a cancelled database operation must remain diagnostic.
func backgroundExecutionDiagnostic(err error) error {
	if err == nil {
		return nil
	}
	if marker, ok := err.(interface{ PersistenceFailure() bool }); ok && marker.PersistenceFailure() {
		return err
	}
	if _, ok := err.(*adk.CancelError); ok || err == context.Canceled || err == adk.ErrStreamCanceled {
		return nil
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		for _, child := range multi.Unwrap() {
			remaining = append(remaining, backgroundExecutionDiagnostic(child))
		}
		return errors.Join(remaining...)
	}
	if child := errors.Unwrap(err); child != nil {
		if backgroundExecutionDiagnostic(child) == nil {
			return nil
		}
	}
	return err
}

func persistExecutionFailureTx(ctx context.Context, tx *sql.Tx, scope TaskScope, cause error) error {
	if cause == nil {
		return nil
	}
	message := cause.Error()
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	var marker interface{ PersistenceFailure() bool }
	persistence := errors.As(cause, &marker) && marker.PersistenceFailure()
	_, err := tx.ExecContext(ctx, `INSERT INTO harness_background_execution_failures(task_id,attempt,error,persistence_failure) VALUES(?,?,?,?) ON CONFLICT(task_id,attempt) DO UPDATE SET error=excluded.error,persistence_failure=excluded.persistence_failure`, scope.Binding.TaskID, scope.Attempt, message, persistence)
	if err != nil {
		return fmt.Errorf("persist background execution failure: %w", err)
	}
	return nil
}

func (s *Service) projectExecutionFailure(ctx context.Context, task *harness.BackgroundTask) error {
	var diagnostic string
	err := s.store.DB().QueryRowContext(ctx, `SELECT error FROM harness_background_execution_failures WHERE task_id=? AND attempt=?`, task.ID, task.Attempt).Scan(&diagnostic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if diagnostic != "" && !strings.Contains(task.Error, diagnostic) {
		if task.Error != "" {
			task.Error += "; "
		}
		task.Error += "execution failure: " + diagnostic
	}
	return nil
}
