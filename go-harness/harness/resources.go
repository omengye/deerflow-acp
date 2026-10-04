package harness

import (
	"context"
	"errors"
)

var ErrResourcesWaiting = errors.New("execution is waiting for MCP resource authorization")
var ErrResourcesIncompatible = errors.New("saved MCP resources cannot be reconstructed")

// MCPResourceDescriptor fixes connection intent and actual discovered tools.
// It deliberately contains no commands, URLs, environment or header values.
type MCPResourceDescriptor struct {
	Version      int      `json:"version"`
	ConfigDigest string   `json:"configDigest"`
	SchemaDigest string   `json:"schemaDigest"`
	Servers      []string `json:"servers"`
	Tools        []string `json:"tools"`
}

// MCPResourceRef is immutable execution input. Connection generations and
// operator grants live separately and never alter the saved engine contract.
type MCPResourceRef struct {
	Version          int                   `json:"version"`
	SessionID        string                `json:"sessionId"`
	OriginGeneration string                `json:"originGeneration"`
	HostRecipeID     string                `json:"hostRecipeId,omitempty"`
	Descriptor       MCPResourceDescriptor `json:"descriptor"`
}

type BackgroundResources struct {
	Version      int64                 `json:"version"`
	Status       string                `json:"status"`
	Reason       string                `json:"reason,omitempty"`
	Descriptor   MCPResourceDescriptor `json:"descriptor"`
	CanAuthorize bool                  `json:"canAuthorize"`
}

// Reattaching a session provides transport; it does not grant an old task new
// resource authority. Both inspected versions are checked during authorization.
type BackgroundResourceAuthorizer interface {
	AuthorizeBackgroundResources(context.Context, TaskActor, string, int64, int64) (BackgroundTask, error)
}
