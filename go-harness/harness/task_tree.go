package harness

import (
	"context"
	"errors"
	"time"
)

var ErrTaskTreeStopped = errors.New("delegated task was cancelled by its owner")

type TaskTreeCancelRequest struct {
	RootID          string `json:"rootId"`
	NodeID          string `json:"nodeId"`
	ExpectedVersion int64  `json:"expectedVersion"`
}
type TaskTreeCanceller interface {
	CancelTaskTree(context.Context, TaskActor, TaskTreeCancelRequest) (TaskTreeNode, error)
}

// TaskTreeController is an optional, read-only view of logical root and role
// invocations. Native background task IDs remain execution references, not tree
// identity: a resumed role may have several native tasks over its lifetime.
type TaskTreeController interface {
	TaskTree(context.Context, TaskActor, TaskTreeQuery) (TaskTreePage, error)
	TaskTreeNode(context.Context, TaskActor, string, string) (TaskTreeNode, error)
}

type TaskTreeQuery struct {
	RootID string `json:"rootId,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// Pages use live keyset pagination, not a point-in-time snapshot. Clients must
// finish a traversal before pruning cached nodes and start from an empty cursor
// to discover newly inserted IDs that sort before an existing cursor.
type TaskTreePage struct {
	SessionID   string         `json:"sessionId"`
	Nodes       []TaskTreeNode `json:"nodes"`
	NextCursor  string         `json:"nextCursor,omitempty"`
	HasMore     bool           `json:"hasMore"`
	Consistency string         `json:"consistency"`
}

type TaskTreeNode struct {
	ID                  string                       `json:"id"`
	RootID              string                       `json:"rootId"`
	RootSessionID       string                       `json:"rootSessionId"`
	ParentID            string                       `json:"parentId,omitempty"`
	InvocationID        string                       `json:"invocationId,omitempty"`
	TaskID              string                       `json:"taskId,omitempty"`
	ContinuationOf      string                       `json:"continuationOf,omitempty"`
	Role                string                       `json:"role"`
	Model               string                       `json:"model,omitempty"`
	Mode                string                       `json:"mode"` // root, foreground, background
	Depth               int                          `json:"depth"`
	Description         string                       `json:"description,omitempty"`
	Status              string                       `json:"status"` // unavailable for unrecorded historical outcomes
	Version             int64                        `json:"version"`
	Attempt             int64                        `json:"attempt"`
	BlockedReason       string                       `json:"blockedReason,omitempty"`
	Waiting             *TaskTreeWaiting             `json:"waiting,omitempty"`
	Resources           *BackgroundResources         `json:"resources,omitempty"`
	Execution           *TaskTreeExecution           `json:"execution,omitempty"`
	BackgroundExecution *TaskTreeBackgroundExecution `json:"backgroundExecution,omitempty"`
	Usage               *TaskTreeUsage               `json:"usage,omitempty"`
	Result              *TaskTreeResult              `json:"result,omitempty"`
	Artifacts           []TaskTreeArtifact           `json:"artifacts,omitempty"`
	AllowedActions      []string                     `json:"allowedActions"`
	CreatedAt           *time.Time                   `json:"createdAt,omitempty"`
	UpdatedAt           *time.Time                   `json:"updatedAt,omitempty"`
}

// Execution is an exact foreground resume reference, never inferred from a
// budget root ID. A root may have multiple continuation executions over time.
type TaskTreeExecution struct {
	RunID     string          `json:"runId"`
	Version   int64           `json:"version"`
	Status    ExecutionStatus `json:"status"`
	Resumable bool            `json:"resumable"`
}

// A foreground role inside a native background task uses its host's exact
// reference. This does not turn that role into a background task or attribute
// its host's artifacts, usage, or terminal status to the inner role.
type TaskTreeBackgroundExecution struct {
	TaskID        string `json:"taskId"`
	Version       int64  `json:"version"`
	Status        string `json:"status"`
	InteractionID string `json:"interactionId,omitempty"`
	Resumable     bool   `json:"resumable"`
}

type TaskTreeWaiting struct {
	Kind        string   `json:"kind"` // permission, task_wait, mixed, suspended, unavailable
	TaskIDs     []string `json:"taskIds,omitempty"`
	IntentCount int      `json:"intentCount,omitempty"`
	Resumable   bool     `json:"resumable"`
}

// Nil Usage means attribution is unavailable, never zero usage. Scope is
// root_total or invocation; root totals include descendants and cannot be
// summed with child usage. Invocation counts only this logical role's own
// admissions across its attempts/continuations, excluding descendants. Model
// tokens come from ledger settlements, retaining the provider/estimate flag;
// held tokens remain reservations. Paused/resumed tools count one admission.
type TaskTreeUsage struct {
	Scope        string `json:"scope"`
	ModelCalls   int64  `json:"modelCalls"`
	ToolCalls    int64  `json:"toolCalls"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	TotalTokens  int64  `json:"totalTokens"`
	HeldTokens   int64  `json:"heldTokens"`
	Estimated    bool   `json:"estimated"`
}

// Nil Result means no safely projected result is available. Text contains only
// the final parent-facing answer, never private model messages or reasoning.
type TaskTreeResult struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// Artifacts contain metadata only. A task-tree reference grants no asset read
// authority; content retrieval must separately authorize the root and asset.
type TaskTreeArtifact struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}
