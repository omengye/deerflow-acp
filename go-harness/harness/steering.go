package harness

import "time"

type SteeringRequest struct {
	RunID           string `json:"runId"`
	ExpectedVersion int64  `json:"expectedVersion"`
	RequestID       string `json:"requestId"`
	Text            string `json:"text"`
}

// Applied means the instruction entered the next root model decision input.
// It is not permission to execute a tool and never targets an active child.
type SteeringRecord struct {
	Sequence  int64     `json:"sequence"`
	RequestID string    `json:"requestId"`
	SessionID string    `json:"sessionId"`
	RunID     string    `json:"runId"`
	Text      string    `json:"text"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SteeringList struct {
	SessionID string           `json:"sessionId"`
	RunID     string           `json:"runId"`
	Items     []SteeringRecord `json:"items"`
}
