// Package harness defines the transport-independent public contract of DeerFlow.
// Eino and ACP types are deliberately confined to their internal adapters.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalidInput      = errors.New("invalid input")
	ErrNotFound          = errors.New("not found")
	ErrBusy              = errors.New("session is busy")
	ErrNotAttached       = errors.New("session is not attached to this connection")
	ErrAttachedElsewhere = errors.New("session is attached to another connection")
	ErrPermissionDenied  = errors.New("permission denied")
)

type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

type Message struct {
	Role    string    `json:"role"`
	Content []Content `json:"content"`
}

type Session struct {
	ID            string    `json:"id"`
	CWD           string    `json:"cwd"`
	Title         string    `json:"title,omitempty"`
	Mode          string    `json:"mode"`
	Model         string    `json:"model"`
	ApprovalMode  string    `json:"approvalMode"`
	Subagents     bool      `json:"subagents"`
	ConfigVersion int64     `json:"configVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type RunRequest struct {
	Session Session
	RunID   string
	InputID string
	Input   []Content
	History []Message
}

type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
	Estimated    bool  `json:"estimated,omitempty"`
}

type RunEvent struct {
	Sequence   int64           `json:"sequence,omitempty"`
	SessionID  string          `json:"sessionId"`
	RunID      string          `json:"runId"`
	Kind       string          `json:"kind"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Status     string          `json:"status,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Content    []Content       `json:"content,omitempty"`
	Usage      *Usage          `json:"usage,omitempty"`
	Receipt    *ToolReceipt    `json:"receipt,omitempty"`
}

type PermissionRequest struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"sessionId"`
	RunID         string          `json:"runId"`
	ConfigVersion int64           `json:"configVersion"`
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Arguments     json.RawMessage `json:"arguments"`
}

type PermissionDecision string

const (
	AllowOnce           PermissionDecision = "allow_once"
	AllowAlways         PermissionDecision = "allow_always"
	RejectOnce          PermissionDecision = "reject_once"
	RejectAlways        PermissionDecision = "reject_always"
	PermissionCancelled PermissionDecision = "cancelled"
)

type PermissionHandler func(context.Context, PermissionRequest) (PermissionDecision, error)
type EventHandler func(context.Context, RunEvent) error

type RunResult struct {
	StopReason string
	Limit      string
}

// Engine must stop model/tool execution when ctx is cancelled and finish cleanup
// before returning. Event handlers may fail, which must also end the run.
type Engine interface {
	Run(context.Context, RunRequest, EventHandler, PermissionHandler) (RunResult, error)
}
