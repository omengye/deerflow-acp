// Package interaction is the neutral trusted seam between runtime persistence
// and an engine's native interrupt/resume implementation. None of these types
// are transport request contracts.
package interaction

import (
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type PermissionIntent struct {
	ID      string
	Version int64
	RoleID  string `json:",omitempty"`
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

// TaskWaitBroker checks the original root and role scope before a foreground
// tool can publish a durable wait. It does not approve descendant effects.
type TaskWaitBroker interface {
	TaskWaitReady(context.Context, TaskWait) (bool, error)
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
	RoleID            string `json:",omitempty"`
}
type StagedExecutionCheckpoint struct {
	ID         string
	Data       []byte
	Remove     bool
	Interrupts []ExecutionInterruptBinding
	TaskWaits  []TaskWaitBinding
}

// ExecutionInputSource is reconstructed only by the runtime from its immutable
// accepted continuation record. It is not a public prompt or tool argument.
// Input contains notification data, never the originating user's prompt.
type ExecutionInputSource struct {
	Kind            string
	SHA             string
	Input           []harness.Content
	PinnedExtension json.RawMessage
	RoleCatalog     json.RawMessage
	RoleContext     *RoleContext
	// Live, host-validated MCP authority for this reconstructed continuation.
	// It is not part of the immutable input or supplied by the client/model.
	ResourceGeneration string
}

// ExecutionHooks carries host-owned native addresses and server-issued grants.
// StageCheckpoint is called only after all engine resources and I/O join; the
// runtime must publish it atomically with the execution state and budget end.
type ExecutionHooks struct {
	Broker        InteractionBroker
	Targets       map[string]PermissionResume
	Cancellations map[string]RoleCancellationResume
	// TaskWaitsEnabled is host-owned and pinned in the checkpoint so older
	// executions retain their bounded task_wait tool contract on resume.
	TaskWaitsEnabled bool
	WaitTargets      map[string]TaskWait
	StageCheckpoint  func(context.Context, StagedExecutionCheckpoint) error
	InputSource      *ExecutionInputSource
}
type executionHooksKey struct{}

func WithExecutionHooks(ctx context.Context, hooks ExecutionHooks) context.Context {
	cloned := make(map[string]PermissionResume, len(hooks.Targets))
	for id, target := range hooks.Targets {
		cloned[id] = target
	}
	hooks.Targets = cloned
	cancellations := make(map[string]RoleCancellationResume, len(hooks.Cancellations))
	for id, target := range hooks.Cancellations {
		cancellations[id] = target
	}
	hooks.Cancellations = cancellations
	waits := make(map[string]TaskWait, len(hooks.WaitTargets))
	for id, wait := range hooks.WaitTargets {
		waits[id] = wait
	}
	hooks.WaitTargets = waits
	if hooks.InputSource != nil {
		source := *hooks.InputSource
		source.PinnedExtension = append(json.RawMessage(nil), source.PinnedExtension...)
		source.RoleCatalog = append(json.RawMessage(nil), source.RoleCatalog...)
		if source.RoleContext != nil {
			data, _ := json.Marshal(source.RoleContext)
			var copied RoleContext
			_ = json.Unmarshal(data, &copied)
			source.RoleContext = &copied
		}
		source.Input = append([]harness.Content(nil), source.Input...)
		for i := range source.Input {
			if source.Input[i].Asset != nil {
				asset := *source.Input[i].Asset
				source.Input[i].Asset = &asset
			}
			if source.Input[i].Size != nil {
				size := *source.Input[i].Size
				source.Input[i].Size = &size
			}
		}
		hooks.InputSource = &source
	}
	return context.WithValue(ctx, executionHooksKey{}, &hooks)
}
func FromContext(ctx context.Context) *ExecutionHooks {
	hooks, _ := ctx.Value(executionHooksKey{}).(*ExecutionHooks)
	return hooks
}
