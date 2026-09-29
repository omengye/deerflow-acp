package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ProcessBackgroundNotification delivers a saved notification to its parent
// model as a durable continuation using the originating run's budget. It does
// not acknowledge the UI inbox or reuse any previous tool approval. Repeating
// the call returns the previously admitted execution without running it again.
// With no approval handler, an ask-policy tool pauses for ResumeExecution.
func (c *Client) ProcessBackgroundNotification(ctx context.Context, sessionID, notificationID string, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.RunResult{}, err
	}
	defer done()
	return c.background.ProcessBackgroundNotification(ctx, actor, notificationID, emit, approve)
}

func (h *backgroundHost) ProcessBackgroundNotification(ctx context.Context, actor harness.TaskActor, notificationID string, emit harness.EventHandler, approve harness.PermissionHandler) (out harness.RunResult, err error) {
	defer func() { err = backgroundAPIError(err) }()
	return h.runtime.ProcessBackgroundNotification(ctx, actor.OwnerID, actor.SessionID, notificationID, emit, approve)
}

var _ harness.BackgroundNotificationProcessor = (*backgroundHost)(nil)
