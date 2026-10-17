package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ExecutionPermissions returns immutable requests to an attached host caller.
// It confers no grant and verifies the same version/manifest used by resume.
func (s *Service) ExecutionPermissions(ctx context.Context, owner, sessionID string, state harness.ExecutionState) ([]harness.PermissionRequest, error) {
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return nil, err
	}
	var result []harness.PermissionRequest
	err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, sessionID, state.RunID)
		if err != nil {
			return err
		}
		if row.State.Status != harness.ExecutionWaitingInput || row.State.Version != state.Version {
			return harness.ErrExecutionConflict
		}
		if err = validateExecutionManifest(ctx, tx, row); err != nil {
			return err
		}
		for _, b := range row.Manifest.Interrupts {
			p, v, status, err := readExecutionIntent(ctx, tx, state.RunID, b.IntentID)
			if err != nil {
				return err
			}
			if status != "pending" || v != b.IntentVersion {
				return harness.ErrExecutionConflict
			}
			result = append(result, p)
		}
		return nil
	})
	return result, err
}
