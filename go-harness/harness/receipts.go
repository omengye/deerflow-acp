package harness

import (
	"errors"
	"time"
)

var (
	ErrReceiptConflict        = errors.New("tool receipt conflicts with recorded execution")
	ErrReconciliationRequired = errors.New("uncertain tool effects require explicit reconciliation")
)

// ToolNotExecutedError certifies that a tool rejected a call before starting
// its operation. Only use it at a verified pre-execution boundary; a failed
// process or a partially applied mutation does not provide this evidence.
type ToolNotExecutedError struct{ cause error }

func (e *ToolNotExecutedError) Error() string { return e.cause.Error() }
func (e *ToolNotExecutedError) Unwrap() error { return e.cause }

// MarkToolNotExecuted preserves the failure while recording that no operation
// was started. Wrappers must preserve the error chain so receipts can use it.
func MarkToolNotExecuted(err error) error {
	if err == nil {
		return nil
	}
	return &ToolNotExecutedError{cause: err}
}

// ToolNoEffectError carries terminal evidence from a native read-only tool.
// It must not be used for a command merely because its process has exited.
type ToolNoEffectError struct{ cause error }

func (e *ToolNoEffectError) Error() string { return e.cause.Error() }
func (e *ToolNoEffectError) Unwrap() error { return e.cause }

func MarkToolNoEffect(err error) error {
	if err == nil {
		return nil
	}
	return &ToolNoEffectError{cause: err}
}

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
// fails is uncertain unless the native tool supplies evidence that it rejected
// the call before execution, or it has a verified read-only contract.
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
