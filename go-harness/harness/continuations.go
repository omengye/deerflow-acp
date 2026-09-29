package harness

import "context"

// BackgroundNotificationProcessor optionally admits an existing notification
// as a durable parent continuation. The host authorizes the currently attached
// actor, preserves notification identity and inherits its originating budget.
// Processing is separate from UI acknowledgement and supplies no new prompt or
// tool authorization. Repeated requests must return the existing execution.
type BackgroundNotificationProcessor interface {
	ProcessBackgroundNotification(context.Context, TaskActor, string, EventHandler, PermissionHandler) (RunResult, error)
}
