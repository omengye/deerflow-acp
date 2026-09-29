package eino

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type PermissionIntent = interaction.PermissionIntent
type PermissionResume = interaction.PermissionResume
type InteractionBroker = interaction.InteractionBroker
type ExecutionInterruptBinding = interaction.ExecutionInterruptBinding
type StagedExecutionCheckpoint = interaction.StagedExecutionCheckpoint
type ExecutionHooks = interaction.ExecutionHooks

func WithExecutionHooks(ctx context.Context, hooks ExecutionHooks) context.Context {
	return interaction.WithExecutionHooks(ctx, hooks)
}
func executionHooks(ctx context.Context) *ExecutionHooks { return interaction.FromContext(ctx) }

// BindExecutionHooks preserves host authority when a native background runner
// supplies its own invocation contexts after PrepareAttempt has returned.
func BindExecutionHooks(agent adk.ResumableAgent, hooks ExecutionHooks) adk.ResumableAgent {
	return &executionHookAgent{ResumableAgent: agent, hooks: hooks}
}

type executionHookAgent struct {
	adk.ResumableAgent
	hooks ExecutionHooks
}

func (a *executionHookAgent) Run(ctx context.Context, input *adk.AgentInput, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.ResumableAgent.Run(WithExecutionHooks(ctx, a.hooks), input, opts...)
}
func (a *executionHookAgent) Resume(ctx context.Context, info *adk.ResumeInfo, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.ResumableAgent.Resume(WithExecutionHooks(ctx, a.hooks), info, opts...)
}

// PermissionInterruptBindings extracts only this adapter's registered native
// permission contexts, rejecting foreign/ambiguous root causes.
func PermissionInterruptBindings(contexts []*adk.InterruptCtx) ([]ExecutionInterruptBinding, error) {
	return collectInterruptBindings(contexts)
}

type permissionInterrupt struct {
	Intent PermissionIntent
	CallID string
}
type permissionState struct {
	Intent                                       PermissionIntent
	RequestHash, RootBudgetID, BudgetOperationID string
}

func init() {
	schema.Register[*permissionInterrupt]()
	schema.Register[permissionState]()
	schema.Register[PermissionResume]()
}

func permissionHash(req harness.PermissionRequest) string {
	data, _ := json.Marshal(req)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
func toolBudgetDigest(name, args string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(name+"\x00"+args)))
}

func (m *toolMiddleware) requestFor(tc *adk.ToolContext, arguments json.RawMessage) harness.PermissionRequest {
	return harness.PermissionRequest{ID: m.sink.request.RunID + "/" + tc.CallID, SessionID: m.sink.request.Session.ID, RunID: m.sink.request.RunID, ConfigVersion: m.sink.request.Session.ConfigVersion, ToolCallID: tc.CallID, ToolName: tc.Name, Arguments: append(json.RawMessage(nil), arguments...)}
}

// admitTool handles the one-time logical tool admission. A native permission
// resume transfers the already-counted paused reservation to the new attempt.
func (m *toolMiddleware) admitTool(ctx context.Context, tc *adk.ToolContext, args string, resume *permissionState) error {
	if m.budget.ledger == nil {
		if resume != nil {
			return nil
		}
		return m.budget.tool()
	}
	req := durablebudget.ToolRequest{OperationID: rand.Text(), Digest: toolBudgetDigest(tc.Name, args)}
	var grant durablebudget.Reservation
	var err error
	if resume != nil {
		grant, err = m.budget.ledger.ResumeTool(ctx, m.budget.scope, req, resume.BudgetOperationID)
	} else {
		grant, err = m.budget.ledger.ReserveTool(ctx, m.budget.scope, req)
	}
	if err != nil {
		return m.budget.classify(err)
	}
	m.grants.Store(tc, grant)
	return nil
}

func (m *toolMiddleware) interruptPermission(ctx context.Context, tc *adk.ToolContext, state permissionState) error {
	if value, ok := m.grants.LoadAndDelete(tc); ok {
		grant := value.(durablebudget.Reservation)
		if err := m.budget.ledger.PauseTool(ctx, grant); err != nil {
			// Keep the grant available for the normal failure cleanup path.
			m.grants.Store(tc, grant)
			return err
		}
		state.BudgetOperationID = grant.OperationID
	}
	// A suspended permission stays a pending durable receipt/card. It has no
	// terminal tool_end, and closeOpen must not manufacture one during join.
	m.sink.mu.Lock()
	delete(m.sink.active, tc.CallID)
	m.sink.mu.Unlock()
	return tool.StatefulInterrupt(ctx, &permissionInterrupt{Intent: state.Intent, CallID: tc.CallID}, state)
}

func (m *toolMiddleware) governedPermission(ctx context.Context, tc *adk.ToolContext, args string, hooks *ExecutionHooks) (harness.PermissionDecision, error) {
	req := m.requestFor(tc, json.RawMessage(args))
	was, has, saved := tool.GetInterruptState[permissionState](ctx)
	if was {
		if !has || saved.Intent.ID == "" || saved.Intent.Version < 1 || saved.RequestHash != permissionHash(req) || saved.RootBudgetID != m.sink.request.RootBudgetID {
			return "", fmt.Errorf("%w: permission checkpoint identity changed", harness.ErrInvalidInput)
		}
		target, hasData, resume := tool.GetResumeContext[PermissionResume](ctx)
		if !target || !hasData {
			return "", m.interruptPermission(ctx, tc, saved)
		}
		if resume.IntentID != saved.Intent.ID || resume.IntentVersion != saved.Intent.Version || resume.GrantID == "" {
			return "", fmt.Errorf("%w: permission resume identity changed", harness.ErrInvalidInput)
		}
		decision, err := hooks.Broker.ResolvePermission(ctx, req, saved.Intent, resume.GrantID)
		if err != nil {
			return "", err
		}
		if decision != harness.AllowOnce && decision != harness.AllowAlways && decision != harness.RejectOnce && decision != harness.RejectAlways {
			return "", fmt.Errorf("%w: permission grant has no definitive decision", harness.ErrInvalidInput)
		}
		if err = m.admitTool(ctx, tc, args, &saved); err != nil {
			return "", err
		}
		return decision, nil
	}
	if err := m.admitTool(ctx, tc, args, nil); err != nil {
		return "", err
	}
	intent, err := hooks.Broker.PreparePermission(ctx, req)
	if err != nil {
		return "", err
	}
	if intent.ID == "" || intent.Version < 1 {
		return "", errors.New("permission broker returned an invalid intent")
	}
	state := permissionState{Intent: intent, RequestHash: permissionHash(req), RootBudgetID: m.sink.request.RootBudgetID}
	return "", m.interruptPermission(ctx, tc, state)
}

func isPermissionInterrupt(err error) bool { _, ok := compose.IsInterruptRerunError(err); return ok }

func collectInterruptBindings(contexts []*adk.InterruptCtx) ([]ExecutionInterruptBinding, error) {
	bindings := make(map[string]ExecutionInterruptBinding)
	intents := make(map[string]string)
	for _, item := range contexts {
		if item == nil {
			continue
		}
		info, ok := item.Info.(*permissionInterrupt)
		if !ok {
			if item.IsRootCause {
				return nil, errors.New("unsupported execution interrupt")
			}
			continue
		}
		if info == nil || info.Intent.ID == "" || info.Intent.Version < 1 || item.ID == "" {
			return nil, errors.New("invalid permission interrupt context")
		}
		binding := ExecutionInterruptBinding{IntentID: info.Intent.ID, IntentVersion: info.Intent.Version, NativeInterruptID: item.ID}
		if prior, ok := bindings[item.ID]; ok && prior != binding {
			return nil, errors.New("conflicting permission interrupt context")
		}
		if prior, ok := intents[info.Intent.ID]; ok && prior != item.ID {
			return nil, errors.New("one permission intent has multiple native targets")
		}
		bindings[item.ID] = binding
		intents[info.Intent.ID] = item.ID
	}
	if len(bindings) == 0 {
		return nil, errors.New("execution interrupt has no permission target")
	}
	result := make([]ExecutionInterruptBinding, 0, len(bindings))
	for _, binding := range bindings {
		result = append(result, binding)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NativeInterruptID < result[j].NativeInterruptID })
	return result, nil
}

func resumeParams(hooks *ExecutionHooks, saved *checkpointEnvelope) (*adk.ResumeParams, error) {
	if saved == nil {
		return nil, nil
	}
	if len(saved.Interrupts) == 0 {
		if hooks != nil && len(hooks.Targets) > 0 {
			return nil, errors.New("resume targets do not belong to a permission checkpoint")
		}
		return nil, nil
	}
	if hooks == nil || hooks.Broker == nil || hooks.StageCheckpoint == nil || len(hooks.Targets) != len(saved.Interrupts) {
		return nil, errors.New("permission checkpoint requires all trusted resume targets")
	}
	params := &adk.ResumeParams{Targets: make(map[string]any, len(hooks.Targets))}
	for _, binding := range saved.Interrupts {
		resume, ok := hooks.Targets[binding.NativeInterruptID]
		if !ok || resume.IntentID != binding.IntentID || resume.IntentVersion != binding.IntentVersion || resume.GrantID == "" {
			return nil, errors.New("resume target does not match permission checkpoint")
		}
		params.Targets[binding.NativeInterruptID] = resume
	}
	return params, nil
}
