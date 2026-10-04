package interaction

import (
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

// RoleContext is emitted by the host's AfterAgent middleware. It carries only
// that role's conversation; it is never a client/model supplied resume input.
type RoleContext struct {
	Invocation agents.Invocation `json:"invocation"`
	SessionID  string            `json:"sessionId"`
	RunID      string            `json:"runId"`
	Messages   json.RawMessage   `json:"messages"`
}

type RoleContextBroker interface {
	StageRoleContext(context.Context, RoleContext) error
}

// RoleContinuationFailure is issued only by the engine after all private role
// I/O and cleanup joined. Runtime must still verify the saved recipient and
// terminal tool receipts before publishing a failure summary to its parent.
type RoleContinuationFailure struct{ Cause error }

func (e *RoleContinuationFailure) Error() string { return "delegated continuation failed" }
func (e *RoleContinuationFailure) Unwrap() error { return e.Cause }

// Every joined branch must be certified. A storage, callback or heartbeat
// error accompanying an ordinary provider failure keeps its normal semantics.
func IsRoleContinuationFailure(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*RoleContinuationFailure); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsRoleContinuationFailure(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsRoleContinuationFailure(wrapped.Unwrap())
	}
	return false
}
