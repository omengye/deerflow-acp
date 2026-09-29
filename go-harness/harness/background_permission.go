package harness

import "context"

// BackgroundPermissionQuery identifies one intent in the exact saved approval
// batch the user is reviewing. It confers no execution authority.
type BackgroundPermissionQuery struct {
	TaskID        string `json:"taskId"`
	InteractionID string `json:"interactionId"`
	TaskVersion   int64  `json:"taskVersion"`
	IntentID      string `json:"intentId"`
}

// BackgroundPermissionReader is an optional, owner-only preview capability.
// Task lists contain compact redacted descriptors. A client uses this method
// to display exact tool arguments before requesting a human decision.
type BackgroundPermissionReader interface {
	BackgroundPermission(context.Context, TaskActor, BackgroundPermissionQuery) (PermissionRequest, error)
}
