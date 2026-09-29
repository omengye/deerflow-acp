// Package interaction is the neutral trusted seam between runtime persistence
// and an engine's native interrupt/resume implementation. None of these types
// are transport request contracts.
package interaction

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type PermissionIntent struct {
	ID      string
	Version int64
}
type PermissionResume struct {
	IntentID      string
	IntentVersion int64
	GrantID       string
}

// ResolvePermission is read-only. The runtime event callback must consume the
// grant in the same SQL transaction as tool_execute or terminal denial.
type InteractionBroker interface {
	PreparePermission(context.Context, harness.PermissionRequest) (PermissionIntent, error)
	ResolvePermission(context.Context, harness.PermissionRequest, PermissionIntent, string) (harness.PermissionDecision, error)
}

// PolicyPermissionBroker optionally resolves a new intent from host-owned
// inherited policy. A handled decision must be definitive and its current-
// attempt grant must already exist; tool_execute or terminal denial consumes
// that grant atomically with its receipt. It confers no foreground actor or
// user approval. Unhandled intents use the normal native interrupt path.
type PolicyPermissionBroker interface {
	ResolvePolicyPermission(context.Context, harness.PermissionRequest, PermissionIntent) (harness.PermissionDecision, bool, error)
}

type ExecutionInterruptBinding struct {
	IntentID          string
	IntentVersion     int64
	NativeInterruptID string
}
type StagedExecutionCheckpoint struct {
	ID         string
	Data       []byte
	Remove     bool
	Interrupts []ExecutionInterruptBinding
}

// ExecutionHooks carries host-owned native addresses and server-issued grants.
// StageCheckpoint is called only after all engine resources and I/O join; the
// runtime must publish it atomically with the execution state and budget end.
type ExecutionHooks struct {
	Broker          InteractionBroker
	Targets         map[string]PermissionResume
	StageCheckpoint func(context.Context, StagedExecutionCheckpoint) error
}
type executionHooksKey struct{}

func WithExecutionHooks(ctx context.Context, hooks ExecutionHooks) context.Context {
	cloned := make(map[string]PermissionResume, len(hooks.Targets))
	for id, target := range hooks.Targets {
		cloned[id] = target
	}
	hooks.Targets = cloned
	return context.WithValue(ctx, executionHooksKey{}, &hooks)
}
func FromContext(ctx context.Context) *ExecutionHooks {
	hooks, _ := ctx.Value(executionHooksKey{}).(*ExecutionHooks)
	return hooks
}
