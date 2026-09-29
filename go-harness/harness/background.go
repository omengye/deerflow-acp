package harness

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrBackgroundUnavailable = errors.New("background execution dependencies are unavailable")
	ErrBackgroundClosed      = errors.New("background service is closing")
	ErrChildSessionBusy      = errors.New("child session already has an active task")
	ErrTaskOriginConflict    = errors.New("background task origin was reused with different intent")
	ErrBackgroundUncertain   = errors.New("background attempt cleanup could not be confirmed")
)

// TaskActor identifies the currently attached caller. The background service
// delegates validation to the host's authorizer; these strings confer no access.
type TaskActor struct{ OwnerID, SessionID string }

// BackgroundController exposes management of already accepted background work.
// Every operation must authorize the actor's current parent-session attachment.
// Submission is intentionally absent: a task requires a live originating tool
// receipt and the host's immutable execution/budget binding.
type BackgroundController interface {
	BackgroundTasks(context.Context, TaskActor, string, int) ([]BackgroundTask, error)
	BackgroundTask(context.Context, TaskActor, string) (BackgroundTask, error)
	WaitBackgroundTask(context.Context, TaskActor, string, int64) (BackgroundTask, error)
	// Cancel requests cancellation; the returned snapshot may still be running.
	CancelBackgroundTask(context.Context, TaskActor, string) (BackgroundTask, error)
	ApproveBackgroundTask(context.Context, TaskActor, string, TaskApproval) (BackgroundTask, error)
	BackgroundNotifications(context.Context, TaskActor, int64, int) ([]BackgroundNotification, error)
	AcknowledgeBackgroundNotification(context.Context, TaskActor, string) error
}

// BackgroundTaskResumer optionally resumes a suspended task at its current
// version. The host must authorize the actor and compare the persisted version
// atomically; this operation cannot approve a waiting permission or supply
// native checkpoint/resume data.
type BackgroundTaskResumer interface {
	ResumeBackgroundTask(context.Context, TaskActor, string, int64) (BackgroundTask, error)
}

// BackgroundTask is a transport-independent projection of Eino's task record.
type BackgroundTask struct {
	ID             string                 `json:"id"`
	SessionID      string                 `json:"sessionId"`
	ChildSessionID string                 `json:"childSessionId,omitempty"`
	Description    string                 `json:"description"`
	Status         string                 `json:"status"`
	Version        int64                  `json:"version"`
	Attempt        int64                  `json:"attempt"`
	Result         []byte                 `json:"result,omitempty"`
	Error          string                 `json:"error,omitempty"`
	BlockedReason  string                 `json:"blockedReason,omitempty"`
	CreatedAt      time.Time              `json:"createdAt"`
	UpdatedAt      time.Time              `json:"updatedAt"`
	Interaction    *BackgroundInteraction `json:"interaction,omitempty"`
}

// BackgroundInteraction exposes descriptive permission intents. Native
// checkpoint addresses and one-use execution grants remain server-owned.
type BackgroundInteraction struct {
	ID            string                 `json:"id"`
	TaskVersion   int64                  `json:"taskVersion"`
	WaitingInputs []ExecutionInteraction `json:"waitingInputs"`
	Resumable     bool                   `json:"resumable"`
	BlockedReason string                 `json:"blockedReason,omitempty"`
}

type BackgroundNotification struct {
	Sequence    int64     `json:"sequence"`
	ID          string    `json:"id"`
	TaskID      string    `json:"taskId"`
	SessionID   string    `json:"sessionId"`
	Kind        string    `json:"kind"`
	TaskVersion int64     `json:"taskVersion"`
	Data        []byte    `json:"data,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

type TaskApproval struct {
	ID          string `json:"id"`
	TaskVersion int64  `json:"taskVersion"`
	// Decision accepts allow_once or reject_once for this saved batch only.
	Decision PermissionDecision `json:"decision"`
	// Evidence is broker-owned structured input, never an arbitrary native
	// Resume target map from an untrusted client.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}
