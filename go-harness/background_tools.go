package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

const backgroundToolWaitLimit = 10 * time.Second

type backgroundToolService interface {
	SubmitNativeSubagent(context.Context, harness.TaskActor, background.Binding, *adk.AgentInput, string) (harness.BackgroundTask, error)
	Get(context.Context, harness.TaskActor, string) (harness.BackgroundTask, error)
	Wait(context.Context, harness.TaskActor, string, int64) (harness.BackgroundTask, error)
	Cancel(context.Context, harness.TaskActor, string, string) (harness.BackgroundTask, error)
}

type backgroundAgentInput struct {
	Instruction    string `json:"instruction" jsonschema:"required,description=Self-contained plain text instructions for an isolated child agent"`
	Description    string `json:"description,omitempty" jsonschema:"description=Short description of this task"`
	ChildSessionID string `json:"childSessionId,omitempty" jsonschema:"description=Optional child session ID returned by an earlier task in this parent session; omit for a new child"`
	Role           string `json:"role,omitempty" jsonschema:"description=Configured role name; omit for general-purpose. The host fixes its model, tools and skills."`
}

type backgroundTaskInput struct {
	TaskID string `json:"taskId" jsonschema:"required,description=Task ID returned by background_agent in this parent session"`
}

type backgroundTaskWaitInput struct {
	TaskID       string `json:"taskId" jsonschema:"required,description=Task ID returned by background_agent in this parent session"`
	AfterVersion int64  `json:"afterVersion" jsonschema:"required,description=Last observed task version; waiting behavior is defined by this tool's description"`
}

type backgroundTool struct {
	resolveRole      func(string) (agents.Spec, error)
	roles            *agents.Catalog
	tree             bool
	mixed            bool
	parentTask       string
	parentInvocation string
	depth            int
	workerRole       string
	worker           bool
	durableWait      bool
	authorizeTarget  func(context.Context, string) error
	info             *schema.ToolInfo
	run              harness.RunRequest
	state            json.RawMessage
	policy           string
	service          backgroundToolService
	authorize        func(context.Context, harness.TaskActor) error
}

// ToolsForRun exposes only parent-session operations. Native child attempts
// have no foreground TaskActor and receive no borrowed parent tools. A parent
// Client MCP is inherited only through the fixed resource reference and an
// independently authorized task lease; no foreground tool closure is copied.
// Engine middleware owns logical receipts, permissions and budget admission.
func (h *backgroundHost) ToolsForRun(ctx context.Context, req harness.RunRequest, state json.RawMessage) ([]tool.BaseTool, error) {
	if h == nil || h.service == nil {
		return nil, nil
	}
	if _, ok := hr.TaskActorFromContext(ctx); !ok {
		return nil, nil
	}
	var resolve func(string) (agents.Spec, error)
	engine, err := h.engine.EngineForRoleContext(ctx)
	if err != nil {
		return nil, err
	}
	policy := h.legacyPolicy
	if engine.HasRoles() {
		var extension extensionState
		if err := json.Unmarshal(state, &extension); err != nil {
			return nil, err
		}
		skills := make([]string, 0, len(extension.Skills))
		for _, ref := range extension.Skills {
			skills = append(skills, ref.Name)
		}
		resolve = func(name string) (agents.Spec, error) { return engine.ResolveRole(name, req.Session.Model, skills) }
		policy = h.policy
	}
	tools, err := newBackgroundTools(ctx, req, state, policy, h.service, h.AuthorizeTaskAccess, resolve)
	if err != nil {
		return nil, err
	}
	if engine.HasRoles() {
		catalog, err := engine.RoleCatalog()
		if err != nil {
			return nil, err
		}
		for _, t := range tools {
			t.(*backgroundTool).roles = catalog
			t.(*backgroundTool).tree = true
			t.(*backgroundTool).mixed = true
			t.(*backgroundTool).depth = 1
		}
	}
	return tools, nil
}

// RoleToolsForRun builds fresh capabilities bound to one live role invocation.
// The connection/worker owns the transport, while the role's durable ancestry
// restricts which tasks the model can create, inspect, wait for or cancel.
func (h *backgroundHost) RoleToolsForRun(ctx context.Context, req harness.RunRequest, state json.RawMessage) ([]tool.BaseTool, error) {
	if h == nil || h.service == nil {
		return nil, nil
	}
	scope, worker := background.ScopeFromContext(ctx)
	invocation, active, err := einoengine.CurrentRole(ctx)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, nil
	}
	mixed := !worker || scope.Binding.AgentVersion == backgroundMixedVersion || scope.Binding.AgentVersion == hr.BackgroundContinuationAgentVersion
	if worker && !mixed && (scope.Binding.AgentVersion != backgroundTreeVersion || invocation.ID != "background/"+scope.Binding.TaskID) {
		return nil, nil
	}
	if invocation.RootID != req.RootBudgetID || worker && (invocation.Depth < scope.Binding.TaskDepth() || req.RunID != scope.Binding.TaskID || req.Session.ID != scope.Binding.ChildSessionID) {
		return nil, harness.ErrPermissionDenied
	}
	engine, err := h.engine.EngineForRoleContext(ctx)
	if err != nil {
		return nil, err
	}
	resolve := func(name string) (agents.Spec, error) {
		return engine.ResolveDescendant(name, invocation.Spec, invocation.Depth)
	}
	tools, err := newBackgroundTools(ctx, req, state, h.policy, h.service, h.AuthorizeTaskAccess, resolve)
	if err != nil {
		return nil, err
	}
	catalog, err := engine.RoleCatalog()
	if err != nil {
		return nil, err
	}
	var selected []tool.BaseTool
	for _, item := range tools {
		t := item.(*backgroundTool)
		t.roles, t.tree, t.mixed, t.parentTask, t.depth = catalog, true, mixed, scope.Binding.TaskID, invocation.Depth+1
		if mixed {
			t.parentInvocation = invocation.ID
		}
		t.authorizeTarget = func(ctx context.Context, id string) error {
			roleID, err := background.TaskRoleID(ctx, h.store.DB(), id)
			if err != nil {
				return err
			}
			if roleID == invocation.ID {
				return harness.ErrNotFound
			}
			allowed, err := agents.IsDescendant(ctx, h.store.DB(), invocation.RootID, invocation.ID, roleID)
			if err != nil {
				return err
			}
			if !allowed {
				return harness.ErrNotFound
			}
			return nil
		}
		if t.info.Name == "background_agent" && (invocation.Depth >= engine.MaxRoleDepth() || len(invocation.Spec.DelegateTo) == 0) {
			continue
		}
		selected = append(selected, t)
	}
	return selected, nil
}

func newBackgroundTools(ctx context.Context, req harness.RunRequest, state json.RawMessage, policy string, service backgroundToolService, authorize func(context.Context, harness.TaskActor) error, resolve ...func(string) (agents.Spec, error)) ([]tool.BaseTool, error) {
	base := backgroundTool{run: req, state: bytes.Clone(state), policy: policy, service: service, authorize: authorize}
	invocation, active, err := einoengine.CurrentRole(ctx)
	if err != nil {
		return nil, err
	}
	if active {
		base.workerRole = invocation.ID
	}
	if _, worker := background.ScopeFromContext(ctx); worker {
		if !active {
			return nil, harness.ErrPermissionDenied
		}
		base.durableWait = true
		base.worker = true
	} else if hooks := interaction.FromContext(ctx); hooks != nil && hooks.TaskWaitsEnabled && hooks.StageCheckpoint != nil {
		base.durableWait = true
	}
	if len(resolve) > 0 {
		base.resolveRole = resolve[0]
	}
	if _, err := base.actor(ctx); err != nil {
		return nil, err
	}
	var pinned extensionState
	if err := decodeBackgroundArguments(string(state), &pinned); err != nil || pinned.Version != 1 {
		return nil, fmt.Errorf("%w: invalid background resource state", harness.ErrInvalidInput)
	}
	status, err := utils.GoStruct2ToolInfo[backgroundTaskInput]("task_status", "Read the current state and result of an authorized background task. A waiting_input task may be waiting for descendants or for human tool approval. Descendant waits resume automatically; the root session handles human approvals. The agent cannot approve tools.")
	if err != nil {
		return nil, err
	}
	wait, err := utils.GoStruct2ToolInfo[backgroundTaskWaitInput]("task_wait", "Wait for a background task version to change, for at most ten seconds. A timeout returns the current state with timedOut=true. Cancellation stops this wait; it does not cancel the task.")
	if err != nil {
		return nil, err
	}
	if base.durableWait {
		wait.Desc = "Suspend this agent until the descendant task finishes. The host saves a checkpoint and releases this worker while waiting. Human approvals are handled by the root session. Cancellation of the parent cancels its descendants."
		if !base.worker {
			wait.Desc = "Suspend this execution until an authorized background task in the same root work finishes. The host saves a checkpoint and releases execution resources while waiting. Human tool approvals remain separate."
		}
	}
	infos := []*schema.ToolInfo{status, wait}
	readOnly := req.Session.Mode == "plan" || req.Session.ApprovalMode == harness.ApprovalReadOnly
	if !readOnly {
		cancel, err := utils.GoStruct2ToolInfo[backgroundTaskInput]("task_cancel", "Request cancellation of a background task in this parent session. Running tasks can remain running until their work and cleanup have joined; inspect the returned state.")
		if err != nil {
			return nil, err
		}
		infos = append(infos, cancel)
	}
	if !readOnly && req.Session.Subagents && pinned.MCPGeneration == "" {
		if req.RootBudgetID == "" || req.RunID == "" || req.Session.ConfigVersion < 1 || policy == "" {
			return nil, harness.ErrBackgroundUnavailable
		}
		agent, err := utils.GoStruct2ToolInfo[backgroundAgentInput]("background_agent", "Start a durable child agent with isolated conversation state and inherited workspace, permissions and total budget. Only explicit text instructions are copied. Return immediately with a task ID; use task_status or task_wait for progress. Human approval can pause the child. Reuse only a childSessionId returned by an earlier task in this parent session.")
		if err != nil {
			return nil, err
		}
		infos = append(infos, agent)
	}
	out := make([]tool.BaseTool, 0, len(infos))
	for _, info := range infos {
		instance := base
		instance.info = info
		out = append(out, &instance)
	}
	return out, nil
}

func (t *backgroundTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *backgroundTool) DurableTaskWait() bool { return t.info.Name == "task_wait" && t.durableWait }

func (t *backgroundTool) actor(ctx context.Context) (harness.TaskActor, error) {
	if err := ctx.Err(); err != nil {
		return harness.TaskActor{}, err
	}
	role, active, roleErr := einoengine.CurrentRole(ctx)
	if roleErr != nil {
		return harness.TaskActor{}, roleErr
	}
	if t.workerRole != "" && (!active || role.ID != t.workerRole || role.RootID != t.run.RootBudgetID) || t.workerRole == "" && active {
		return harness.TaskActor{}, harness.ErrPermissionDenied
	}
	if t.worker {
		workerService, ok := t.service.(interface {
			WorkerActor(context.Context) (harness.TaskActor, error)
		})
		if !ok {
			return harness.TaskActor{}, harness.ErrPermissionDenied
		}
		actor, err := workerService.WorkerActor(ctx)
		if err != nil {
			return harness.TaskActor{}, err
		}
		if actor.SessionID != t.run.Session.ID {
			return harness.TaskActor{}, harness.ErrPermissionDenied
		}
		return actor, nil
	}
	actor, ok := hr.TaskActorFromContext(ctx)
	if !ok || actor.SessionID != t.run.Session.ID || t.service == nil || t.authorize == nil {
		return harness.TaskActor{}, harness.ErrPermissionDenied
	}
	if err := t.authorize(ctx, actor); err != nil {
		return harness.TaskActor{}, err
	}
	return actor, nil
}

func (t *backgroundTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	actor, err := t.actor(ctx)
	if err != nil {
		return "", backgroundAdmissionError(err)
	}
	var task harness.BackgroundTask
	switch t.info.Name {
	case "background_agent":
		var in backgroundAgentInput
		if err = decodeBackgroundArguments(args, &in); err != nil || !backgroundText(in.Instruction, 64<<10, false) || !backgroundText(in.Description, 4096, true) || !backgroundText(in.ChildSessionID, 4096, true) {
			return "", harness.ErrInvalidInput
		}
		callID := compose.GetToolCallID(ctx)
		if callID == "" {
			return "", fmt.Errorf("%w: background submission requires an active tool call", harness.ErrPermissionDenied)
		}
		spec := hr.BackgroundExecutionSpec{Version: 1, Parent: t.run.Session, AgentVersion: backgroundAgentVersion, HostPolicy: t.policy, Input: []harness.Content{{Type: "text", Text: in.Instruction}}, Extension: bytes.Clone(t.state), OriginArguments: args}
		if t.resolveRole != nil {
			role, roleErr := t.resolveRole(in.Role)
			if roleErr != nil {
				return "", harness.MarkToolNotExecuted(roleErr)
			}
			spec.Version, spec.AgentVersion, spec.Role = 2, backgroundRoleVersion, &role
			if t.roles != nil {
				spec.Version, spec.AgentVersion, spec.Roles = 3, backgroundCatalogVersion, t.roles
				if t.tree {
					spec.Version, spec.AgentVersion, spec.Depth, spec.ParentTaskID = 4, backgroundTreeVersion, t.depth, t.parentTask
					if t.mixed {
						spec.Version, spec.AgentVersion, spec.ParentInvocationID = 5, backgroundMixedVersion, t.parentInvocation
						spec.Automatic = hr.ContinuationPolicy(t.roles)
					}
				}
			}
		} else if in.Role != "" {
			return "", harness.MarkToolNotExecuted(harness.ErrInvalidInput)
		}
		contract, contractErr := spec.Contract()
		if contractErr != nil {
			return "", contractErr
		}
		binding := background.Binding{ParentSessionID: actor.SessionID, SubmittedBy: actor.OwnerID, ChildSessionID: in.ChildSessionID, OriginRunID: t.run.RunID, OriginToolCallID: callID, Workspace: t.run.Session.CWD, ExecutionContract: contract, RootBudgetID: t.run.RootBudgetID, AgentVersion: spec.AgentVersion, ConfigVersion: t.run.Session.ConfigVersion}
		binding.ParentInvocationID = spec.ParentInvocationID
		ctx = context.WithValue(ctx, backgroundSpecKey{}, spec)
		task, err = t.service.SubmitNativeSubagent(ctx, actor, binding, &adk.AgentInput{Messages: []*schema.Message{schema.UserMessage(in.Instruction)}}, in.Description)
		if task.ID == "" && errors.Is(err, agents.ErrTaskLimit) {
			err = harness.MarkToolNotExecuted(err)
		}
	case "task_status", "task_cancel":
		var in backgroundTaskInput
		if err = decodeBackgroundArguments(args, &in); err != nil || !backgroundText(in.TaskID, 4096, false) {
			return "", harness.ErrInvalidInput
		}
		if t.authorizeTarget != nil {
			if err = t.authorizeTarget(ctx, in.TaskID); err != nil {
				return "", backgroundAdmissionError(err)
			}
		}
		if t.info.Name == "task_status" {
			task, err = t.service.Get(ctx, actor, in.TaskID)
		} else {
			task, err = t.service.Cancel(ctx, actor, in.TaskID, "cancelled by parent agent")
		}
	case "task_wait":
		var in backgroundTaskWaitInput
		if err = decodeBackgroundArguments(args, &in); err != nil || !backgroundText(in.TaskID, 4096, false) || in.AfterVersion < 1 {
			return "", harness.ErrInvalidInput
		}
		if t.authorizeTarget != nil {
			if err = t.authorizeTarget(ctx, in.TaskID); err != nil {
				return "", backgroundAdmissionError(err)
			}
		}
		if t.durableWait {
			wait := interaction.TaskWait{TaskID: in.TaskID, AfterVersion: in.AfterVersion, CallID: compose.GetToolCallID(ctx), SessionID: t.run.Session.ID, RunID: t.run.RunID, RootBudgetID: t.run.RootBudgetID}
			if t.mixed {
				wait.RoleID = t.workerRole
			}
			was, has, saved := tool.GetInterruptState[interaction.TaskWait](ctx)
			if wait.CallID == "" || was && (!has || saved != wait) {
				return "", harness.ErrTaskOriginConflict
			}
			ready := true
			if !t.worker {
				hooks := interaction.FromContext(ctx)
				if hooks == nil {
					return "", harness.ErrPermissionDenied
				}
				broker, ok := hooks.Broker.(interaction.TaskWaitBroker)
				if !ok {
					return "", harness.ErrPermissionDenied
				}
				if ready, err = broker.TaskWaitReady(ctx, wait); err != nil {
					return "", backgroundAdmissionError(err)
				}
			}
			task, err = t.service.Get(ctx, actor, in.TaskID)
			if err != nil {
				return "", err
			}
			if ready && task.BlockedReason == "" && (task.Status == "completed" || task.Status == "failed" || task.Status == "canceled") {
				if err = t.observeTaskResult(ctx, task, wait); err != nil {
					return "", err
				}
				return backgroundToolResult(task, false, nil)
			}
			return "", tool.StatefulInterrupt(ctx, &interaction.TaskWaitInterrupt{Wait: wait}, wait)
		}
		waitCtx, cancel := context.WithTimeout(ctx, backgroundToolWaitLimit)
		task, err = t.service.Wait(waitCtx, actor, in.TaskID, in.AfterVersion)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			// Re-read with fresh authorization; neither a timeout nor a stale
			// attachment can be presented as successful task completion.
			readCtx, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			task, err = t.service.Get(readCtx, actor, in.TaskID)
			return backgroundToolResult(task, true, err)
		}
	default:
		return "", harness.ErrInvalidInput
	}
	if err == nil && task.BlockedReason == "" && (task.Status == "completed" || task.Status == "failed" || task.Status == "canceled") && (t.info.Name == "task_status" || t.info.Name == "task_cancel") {
		wait := interaction.TaskWait{TaskID: task.ID, AfterVersion: task.Version, CallID: compose.GetToolCallID(ctx), SessionID: t.run.Session.ID, RunID: t.run.RunID, RootBudgetID: t.run.RootBudgetID, RoleID: t.workerRole}
		if err = t.observeTaskResult(ctx, task, wait); err != nil {
			return "", err
		}
	}
	return backgroundToolResult(task, false, err)
}

func decodeBackgroundArguments(args string, into any) error {
	if len(args) > 128<<10 || !strings.HasPrefix(strings.TrimSpace(args), "{") {
		return harness.ErrInvalidInput
	}
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.DisallowUnknownFields()
	if decoder.Decode(into) != nil {
		return harness.ErrInvalidInput
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return harness.ErrInvalidInput
	}
	return nil
}

func (t *backgroundTool) observeTaskResult(ctx context.Context, task harness.BackgroundTask, wait interaction.TaskWait) error {
	if hooks := interaction.FromContext(ctx); hooks != nil {
		if observer, ok := hooks.Broker.(interaction.TaskResultObserver); ok {
			return observer.ObserveTaskResult(ctx, interaction.TaskResultDelivery{Wait: wait, TaskVersion: task.Version})
		}
	}
	return nil
}

func backgroundAdmissionError(err error) error {
	// Only known authorization rejections certify a pre-effect denial. A role
	// lease/storage error must remain fatal even though no task call was made.
	switch err {
	case harness.ErrPermissionDenied, harness.ErrNotAttached, harness.ErrNotFound:
		return harness.MarkToolNotExecuted(err)
	default:
		return err
	}
}

func backgroundText(value string, max int, optional bool) bool {
	return len(value) <= max && !strings.ContainsRune(value, 0) && (optional && value == "" || strings.TrimSpace(value) != "")
}

type backgroundToolResultError struct {
	cause  error
	result string
}

func (e *backgroundToolResultError) Error() string      { return e.cause.Error() }
func (e *backgroundToolResultError) Unwrap() error      { return e.cause }
func (e *backgroundToolResultError) ToolResult() string { return e.result }

func backgroundToolResult(task harness.BackgroundTask, timedOut bool, err error) (string, error) {
	if err != nil && task.ID == "" {
		return "", err
	}
	var result any
	if len(task.Result) > 0 {
		if json.Valid(task.Result) {
			result = json.RawMessage(task.Result)
		} else {
			result = string(task.Result)
		}
	}
	data, marshalErr := json.Marshal(struct {
		harness.BackgroundTask
		Result   any  `json:"result,omitempty"`
		TimedOut bool `json:"timedOut,omitempty"`
	}{task, result, timedOut})
	if marshalErr != nil {
		return "", errors.Join(err, marshalErr)
	}
	if err != nil {
		// Creation can commit before the notification transport fails. Preserve
		// the accepted ID in the tool receipt so its owner can inspect/cancel it.
		return "", &backgroundToolResultError{cause: err, result: string(data)}
	}
	return string(data), nil
}

var _ tool.InvokableTool = (*backgroundTool)(nil)
