package harness

import (
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

// BackgroundTask is a transport-independent projection of Eino's task record.
type BackgroundTask struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"sessionId"`
	ChildSessionID string    `json:"childSessionId,omitempty"`
	Description    string    `json:"description"`
	Status         string    `json:"status"`
	Version        int64     `json:"version"`
	Attempt        int64     `json:"attempt"`
	Result         []byte    `json:"result,omitempty"`
	Error          string    `json:"error,omitempty"`
	BlockedReason  string    `json:"blockedReason,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
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
	ID          string             `json:"id"`
	TaskVersion int64              `json:"taskVersion"`
	Decision    PermissionDecision `json:"decision"`
	// Evidence is broker-owned structured input, never an arbitrary native
	// Resume target map from an untrusted client.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}
