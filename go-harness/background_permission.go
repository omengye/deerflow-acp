package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (h *backgroundHost) BackgroundPermission(ctx context.Context, actor harness.TaskActor, query harness.BackgroundPermissionQuery) (out harness.PermissionRequest, err error) {
	defer func() { err = backgroundAPIError(err) }()
	binding, err := h.service.BindingForTask(ctx, actor, query.TaskID)
	if err != nil {
		return out, err
	}
	return h.interactions.Permission(ctx, actor, binding, query)
}

// BackgroundPermission reads the saved tool arguments for a pending approval.
// Call this to render the actual operation before ApproveBackgroundTask. Reading
// neither consumes the batch nor grants execution; stale batches are rejected.
func (c *Client) BackgroundPermission(ctx context.Context, sessionID string, query harness.BackgroundPermissionQuery) (harness.PermissionRequest, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.PermissionRequest{}, err
	}
	defer done()
	return c.background.BackgroundPermission(ctx, actor, query)
}

var _ harness.BackgroundPermissionReader = (*backgroundHost)(nil)
