package harness

import (
	"errors"
	"time"
)

var (
	ErrReceiptConflict        = errors.New("tool receipt conflicts with recorded execution")
	ErrReconciliationRequired = errors.New("uncertain tool effects require explicit reconciliation")
)

type ReceiptState string

const (
	ReceiptPending     ReceiptState = "pending"
	ReceiptStarted     ReceiptState = "started"
	ReceiptCompleted   ReceiptState = "completed"
	ReceiptNotExecuted ReceiptState = "not_executed"
	ReceiptUncertain   ReceiptState = "uncertain"
	ReceiptNoEffect    ReceiptState = "no_effect"
)

// ToolReceipt records evidence about one attempt. A started operation that
// fails is uncertain even when its provider reports an error or cancellation.
// ArgumentsSummary contains parameter names/types, never argument values.
type ToolReceipt struct {
	SessionID        string         `json:"sessionId"`
	RunID            string         `json:"runId"`
	ToolCallID       string         `json:"toolCallId"`
	ToolName         string         `json:"toolName"`
	ArgumentsDigest  string         `json:"argumentsDigest"`
	ArgumentsSummary string         `json:"argumentsSummary"`
	ConfigVersion    int64          `json:"configVersion"`
	Attempt          int            `json:"attempt"`
	State            ReceiptState   `json:"state"`
	Result           []Content      `json:"result,omitempty"`
	ResultTruncated  bool           `json:"resultTruncated,omitempty"`
	Error            string         `json:"error,omitempty"`
	Version          int64          `json:"version"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	Review           *ReceiptReview `json:"review,omitempty"`
}

// ToolReconciliation records a human's evidence; it never executes the tool.
// ExpectedVersion prevents stale reviews from overwriting newer evidence.
type ToolReconciliation struct {
	RunID           string       `json:"runId"`
	ToolCallID      string       `json:"toolCallId"`
	ExpectedVersion int64        `json:"expectedVersion"`
	Outcome         ReceiptState `json:"outcome"`
	Reviewer        string       `json:"reviewer"`
	Note            string       `json:"note"`
	Result          []Content    `json:"result,omitempty"`
}

type ReceiptReview struct {
	PreviousState   ReceiptState `json:"previousState"`
	Outcome         ReceiptState `json:"outcome"`
	Reviewer        string       `json:"reviewer"`
	Note            string       `json:"note"`
	ReviewedAt      time.Time    `json:"reviewedAt"`
	Result          []Content    `json:"result,omitempty"`
	ResultTruncated bool         `json:"resultTruncated,omitempty"`
}
