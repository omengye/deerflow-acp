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
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
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
}

type backgroundTaskInput struct {
	TaskID string `json:"taskId" jsonschema:"required,description=Task ID returned by background_agent in this parent session"`
}

type backgroundTaskWaitInput struct {
	TaskID       string `json:"taskId" jsonschema:"required,description=Task ID returned by background_agent in this parent session"`
	AfterVersion int64  `json:"afterVersion" jsonschema:"required,description=Last observed task version; wait at most ten seconds for a newer version"`
}

type backgroundTool struct {
	info      *schema.ToolInfo
	run       harness.RunRequest
	state     json.RawMessage
	policy    string
	service   backgroundToolService
	authorize func(context.Context, harness.TaskActor) error
}

// ToolsForRun exposes only parent-session operations. Native child attempts
// have no foreground TaskActor and receive no borrowed parent tools. A parent
// MCP generation is connection-owned and cannot currently be rebuilt as a child
// capability, so background_agent is omitted for that resource configuration.
// Engine middleware owns logical receipts, permissions and budget admission.
func (h *backgroundHost) ToolsForRun(ctx context.Context, req harness.RunRequest, state json.RawMessage) ([]tool.BaseTool, error) {
	if h == nil || h.service == nil {
		return nil, nil
	}
	if _, ok := hr.TaskActorFromContext(ctx); !ok {
		return nil, nil
	}
	return newBackgroundTools(ctx, req, state, h.policy, h.service, h.AuthorizeTaskAccess)
}

func newBackgroundTools(ctx context.Context, req harness.RunRequest, state json.RawMessage, policy string, service backgroundToolService, authorize func(context.Context, harness.TaskActor) error) ([]tool.BaseTool, error) {
	base := backgroundTool{run: req, state: bytes.Clone(state), policy: policy, service: service, authorize: authorize}
	if _, err := base.actor(ctx); err != nil {
		return nil, err
	}
	var pinned extensionState
	if err := decodeBackgroundArguments(string(state), &pinned); err != nil || pinned.Version != 1 {
		return nil, fmt.Errorf("%w: invalid background resource state", harness.ErrInvalidInput)
	}
	status, err := utils.GoStruct2ToolInfo[backgroundTaskInput]("task_status", "Read the current state and result of a background task belonging to this parent session. A waiting_input task requires the user's approval through the host; the agent cannot approve it.")
	if err != nil {
		return nil, err
	}
	wait, err := utils.GoStruct2ToolInfo[backgroundTaskWaitInput]("task_wait", "Wait for a background task version to change, for at most ten seconds. A timeout returns the current state with timedOut=true. Cancellation stops this wait; it does not cancel the task.")
	if err != nil {
		return nil, err
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

func (t *backgroundTool) actor(ctx context.Context) (harness.TaskActor, error) {
	if err := ctx.Err(); err != nil {
		return harness.TaskActor{}, err
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
		return "", err
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
		contract, contractErr := spec.Contract()
		if contractErr != nil {
			return "", contractErr
		}
		binding := background.Binding{ParentSessionID: actor.SessionID, SubmittedBy: actor.OwnerID, ChildSessionID: in.ChildSessionID, OriginRunID: t.run.RunID, OriginToolCallID: callID, Workspace: t.run.Session.CWD, ExecutionContract: contract, RootBudgetID: t.run.RootBudgetID, AgentVersion: backgroundAgentVersion, ConfigVersion: t.run.Session.ConfigVersion}
		ctx = context.WithValue(ctx, backgroundSpecKey{}, spec)
		task, err = t.service.SubmitNativeSubagent(ctx, actor, binding, &adk.AgentInput{Messages: []*schema.Message{schema.UserMessage(in.Instruction)}}, in.Description)
	case "task_status", "task_cancel":
		var in backgroundTaskInput
		if err = decodeBackgroundArguments(args, &in); err != nil || !backgroundText(in.TaskID, 4096, false) {
			return "", harness.ErrInvalidInput
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
