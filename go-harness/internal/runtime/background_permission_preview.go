package runtime

import (
	"context"
	"database/sql"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

// Permission previews one exact pending request without creating a grant,
// changing the inbox, or copying raw arguments into public task listings.
func (s *BackgroundInteractionStore) Permission(ctx context.Context, actor harness.TaskActor, binding background.Binding, query harness.BackgroundPermissionQuery) (harness.PermissionRequest, error) {
	if err := s.authorize(ctx, actor, binding); err != nil {
		return harness.PermissionRequest{}, err
	}
	if query.TaskID != binding.TaskID || query.InteractionID == "" || query.IntentID == "" || query.TaskVersion < 1 {
		return harness.PermissionRequest{}, harness.ErrInvalidInput
	}
	var result harness.PermissionRequest
	err := withExecutionTransaction(ctx, s.store, func(tx *sql.Tx) error {
		if err := s.authorize(ctx, actor, binding); err != nil {
			return err
		}
		task, err := readBackgroundNativeTask(ctx, tx, binding.TaskID)
		if err != nil {
			return err
		}
		if task.Status != bt.StatusWaitingInput || task.Version != query.TaskVersion {
			return harness.ErrExecutionConflict
		}
		m, state, err := s.readManifest(ctx, tx, binding)
		if err != nil {
			return err
		}
		if state != "waiting" || m.ID != query.InteractionID || m.TaskVersion != query.TaskVersion || m.Attempt != task.Attempt {
			return harness.ErrExecutionConflict
		}
		if err = s.validateManifest(ctx, tx, m, task); err != nil {
			return err
		}
		for _, item := range m.Interrupts {
			if item.IntentID != query.IntentID {
				continue
			}
			request, version, intentState, err := readBackgroundIntent(ctx, tx, binding.TaskID, query.IntentID)
			if err != nil {
				return err
			}
			if intentState != "pending" || version != item.IntentVersion {
				return harness.ErrExecutionConflict
			}
			result = request
			return nil
		}
		return harness.ErrNotFound
	})
	if err != nil {
		return harness.PermissionRequest{}, err
	}
	return result, nil
}
