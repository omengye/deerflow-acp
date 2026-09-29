package harness

import (
	"errors"
	"time"
)

var (
	ErrExecutionConflict     = errors.New("execution state or attempt changed")
	ErrExecutionWaitingInput = errors.New("execution is waiting for input")
	ErrExecutionUnresumable  = errors.New("execution checkpoint cannot be resumed safely")
)

type ExecutionStatus string

const (
	ExecutionRunning             ExecutionStatus = "running"
	ExecutionSuspending          ExecutionStatus = "suspending"
	ExecutionWaitingInput        ExecutionStatus = "waiting_input"
	ExecutionResuming            ExecutionStatus = "resuming"
	ExecutionCompleted           ExecutionStatus = "completed"
	ExecutionCancelled           ExecutionStatus = "cancelled"
	ExecutionFailed              ExecutionStatus = "failed"
	ExecutionNeedsReconciliation ExecutionStatus = "needs_reconciliation"
)

// ExecutionInteraction is descriptive. It carries neither a native Eino
// resume address nor a transferable authorization to execute a tool.
type ExecutionInteraction struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Version          int64  `json:"version"`
	ToolCallID       string `json:"toolCallId"`
	ToolName         string `json:"toolName"`
	ArgumentsDigest  string `json:"argumentsDigest"`
	ArgumentsSummary string `json:"argumentsSummary"`
	ConfigVersion    int64  `json:"configVersion"`
}

type ExecutionState struct {
	SessionID     string                 `json:"sessionId"`
	RunID         string                 `json:"runId"`
	InputID       string                 `json:"inputId"`
	Status        ExecutionStatus        `json:"status"`
	Version       int64                  `json:"version"`
	Attempt       int64                  `json:"attempt"`
	AttemptID     string                 `json:"attemptId"`
	WaitingInputs []ExecutionInteraction `json:"waitingInputs,omitempty"`
	EventCursor   int64                  `json:"eventCursor"`
	Resumable     bool                   `json:"resumable"`
	BlockedReason string                 `json:"blockedReason,omitempty"`
	CreatedAt     time.Time              `json:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt"`
}

// ResumeExecutionRequest identifies an existing logical run. Resume never
// accepts replacement prompt text, a checkpoint key, or native target data.
type ResumeExecutionRequest struct {
	RunID           string `json:"runId"`
	ExpectedVersion int64  `json:"expectedVersion"`
}

type CancelExecutionRequest struct {
	RunID           string `json:"runId"`
	ExpectedVersion int64  `json:"expectedVersion"`
}

// ExecutionStopKind distinguishes losing the controlling connection from an
// explicit cancellation. A disconnected waiting interaction is not a denial.
type ExecutionStopKind string

const (
	ExecutionStopCompleted    ExecutionStopKind = "completed"
	ExecutionStopFailed       ExecutionStopKind = "failed"
	ExecutionStopCancelled    ExecutionStopKind = "cancelled"
	ExecutionStopDisconnected ExecutionStopKind = "disconnected"
)
