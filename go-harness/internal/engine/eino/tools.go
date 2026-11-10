package eino

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	acpclient "github.com/omengye/deerflow-acp/go-harness/internal/acp/client"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

type toolMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	sink            *eventSink
	permissions     harness.PermissionHandler
	protected       map[string]bool
	trusted         map[string]bool
	preparers       map[string]harness.ToolArgumentPreparer
	policy          *harness.ToolPolicy
	permissionMode  harness.PermissionMode
	readOnly        bool
	durableTaskWait bool
	io              *runIO
	budget          *runBudget
	images          harness.ToolImageImporter
	outputs         harness.ToolOutputStore
	grants          sync.Map // *adk.ToolContext -> durablebudget.Reservation
	executing       sync.Map // *adk.ToolContext -> started effect, until terminal receipt
	permits         sync.Map // *adk.ToolContext -> release after terminal receipt and cleanup
	outputMu        sync.Mutex
	outputBytes     int
}

// Run after tool-providing middleware so a role's allowlist also governs
// dynamically injected tools, not just ToolsNodeConfig.
func (m *toolMiddleware) BeforeAgent(ctx context.Context, input *adk.ChatModelAgentContext[*schema.Message]) (context.Context, *adk.ChatModelAgentContext[*schema.Message], error) {
	if m.trusted == nil {
		m.trusted = make(map[string]bool)
	}
	if m.protected == nil {
		m.protected = make(map[string]bool)
	}
	copy := *input
	copy.Tools = make([]tool.BaseTool, 0, len(input.Tools))
	available := make(map[string]bool, len(input.Tools))
	delegated := IsDelegatedToolContext(ctx)
	if delegated {
		copy.Instruction += "\n\nUser questions belong to the parent agent. If user information or a decision is missing, return the question to the parent so it can ask the user. Do not call ask_user_question from this delegated task."
	}
	for _, t := range input.Tools {
		info, err := t.Info(ctx)
		if err != nil {
			return ctx, nil, err
		}
		if info == nil || info.Name == "" {
			return ctx, nil, harness.ErrInvalidInput
		}
		// DeepAgent's default child shares the root's tools and middleware.
		// Filter per invocation rather than mutating that shared tool set.
		if delegated && info.Name == "ask_user_question" && toolidentity.IsBuiltin(t, info.Name) {
			continue
		}
		if m.policy != nil && !m.policy.Allows(info.Name) {
			continue
		}
		// Only the engine-reserved DeepAgent tools lack a native constructor
		// marker. All other dynamically supplied tools must prove provenance.
		switch info.Name {
		case "task", "skill", "write_todos":
			m.protected[info.Name] = m.readOnly || m.permissionMode != "" && m.permissionMode.RequiresToolPermission(info.Name, m.trusted[info.Name])
		default:
			m.trusted[info.Name] = toolidentity.IsBuiltin(t, info.Name)
			m.protected[info.Name] = m.readOnly || m.permissionMode.RequiresToolPermission(info.Name, m.trusted[info.Name])
		}
		copy.Tools = append(copy.Tools, t)
		available[info.Name] = true
	}
	return context.WithValue(ctx, availableToolsKey{}, available), &copy, nil
}

func (m *toolMiddleware) start(ctx context.Context, tc *adk.ToolContext, args string) error {
	arguments := json.RawMessage(args)
	if !json.Valid(arguments) {
		return errors.New("tool arguments are not valid JSON")
	}
	if m.policy != nil && !m.policy.Allows(tc.Name) {
		return harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
	}
	if m.nativeDelegation(ctx, tc) {
		// Eino's task endpoint is a resumable orchestration boundary. Its child
		// models and actual tools own the effect admissions and receipts. Giving
		// this boundary a started effect receipt would make every safe child
		// permission suspension look like an unjoined external side effect.
		// The native task tool owns its interrupt state as well. A permission
		// interrupt here would be mistaken for its own child interrupt, so the
		// boundary uses the live session broker; child effects remain durable.
		was, _, _ := tool.GetInterruptState[any](ctx)
		if tc.Name == "task" && m.requiresPermission(tc.Name) && !was {
			// Persist a terminal receipt for the orchestration boundary before
			// entering the child. The child may suspend; an open parent receipt
			// would otherwise be classified as an uncertain external effect.
			if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "pending", Arguments: append(json.RawMessage(nil), arguments...)}); err != nil {
				return err
			}
			decision := harness.RejectOnce
			var err error
			if m.permissions != nil {
				decision, err = m.askPermission(ctx, m.requestFor(tc, arguments))
			}
			if err != nil || (decision != harness.AllowOnce && decision != harness.AllowAlways) {
				failure := harness.ErrPermissionDenied
				if err != nil {
					failure = err
				} else if decision == harness.PermissionCancelled {
					failure = context.Canceled
				}
				if endErr := m.sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: "tool_end", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "failed", Content: []harness.Content{{Type: "text", Text: failure.Error()}}, Receipt: &harness.ToolReceipt{Error: failure.Error(), FailureCode: toolFailureCode(failure)}}); endErr != nil {
					return errors.Join(failure, endErr)
				}
				return failure
			}
			if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_execute", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress"}); err != nil {
				return err
			}
			if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "completed"}); err != nil {
				return err
			}
		}
		m.sink.mu.Lock()
		if m.sink.delegations == nil {
			m.sink.delegations = make(map[string]bool)
		}
		m.sink.delegations[tc.CallID] = true
		m.sink.mu.Unlock()
		kind := "subagent_start"
		if was {
			kind = "subagent_resumed"
		}
		if tc.Name == "task_wait" {
			kind = "background_wait_start"
		}
		return m.sink.emit(ctx, harness.RunEvent{Kind: kind, ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress"})
	}
	hooks := executionHooks(ctx)
	governed := m.requiresPermission(tc.Name) && hooks != nil && hooks.Broker != nil
	wasInterrupted, _, _ := tool.GetInterruptState[permissionState](ctx)
	if governed && wasInterrupted {
		// The first attempt already persisted this pending logical call. Resume
		// changes its state; it does not create another pending receipt/event.
		m.sink.mu.Lock()
		m.sink.active[tc.CallID] = tc.Name
		m.sink.mu.Unlock()
	} else if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "pending", Arguments: append(json.RawMessage(nil), arguments...)}); err != nil {
		return err
	}
	if !governed {
		if err := m.admitTool(ctx, tc, args, nil); err != nil {
			return err
		}
	}
	if m.requiresPermission(tc.Name) {
		var decision harness.PermissionDecision
		var err error
		if governed {
			if hooks.StageCheckpoint == nil {
				return errors.New("durable permission broker requires checkpoint staging")
			}
			endApproval := beginRoleActivity(ctx, "approval")
			decision, err = m.governedPermission(ctx, tc, args, hooks)
			endApproval()
		} else {
			if m.permissions == nil {
				return harness.ErrPermissionDenied
			}
			decision, err = m.askPermission(ctx, m.requestFor(tc, arguments))
		}
		if err != nil {
			return err
		}
		switch decision {
		case harness.AllowOnce, harness.AllowAlways:
		case harness.PermissionCancelled:
			return context.Canceled
		default:
			return harness.ErrPermissionDenied
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Human interaction does not occupy a role's effect-I/O slot. In
	// particular a question must not prevent sibling roles from making progress.
	if !m.userQuestion(tc.Name) {
		release, err := beginRoleWork(ctx)
		if err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return harness.MarkToolNotExecuted(ctx.Err())
			}
			return err
		}
		m.permits.Store(tc, release)
	}
	if value, ok := m.grants.Load(tc); ok {
		first, err := m.budget.ledger.MarkDispatched(ctx, value.(durablebudget.Reservation))
		if err != nil {
			return m.budget.classify(err)
		}
		if !first {
			return durablebudget.ErrConflict
		}
	}
	if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_execute", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress"}); err != nil {
		return err
	}
	m.executing.Store(tc, beginRoleActivity(ctx, "tool"))
	return nil
}

func (m *toolMiddleware) finish(ctx context.Context, tc *adk.ToolContext, content []harness.Content, err error, agentRecovery ...bool) error {
	recoverable := len(agentRecovery) > 0 && agentRecovery[0] && m.recoverableFailure(ctx, tc, err)
	if done, started := m.executing.LoadAndDelete(tc); started {
		done.(func())()
		if !recoverable {
			m.io.recordUnconfirmedEffect(err)
		}
	}
	if m.nativeDelegation(ctx, tc) {
		m.sink.mu.Lock()
		started := m.sink.delegations[tc.CallID]
		m.sink.mu.Unlock()
		if !started {
			return nil
		}
		kind, status := "subagent_end", "completed"
		if isPermissionInterrupt(err) {
			kind, status = "subagent_suspended", "waiting_input"
			content = nil
		} else if err != nil {
			status = "failed"
			content = []harness.Content{{Type: "text", Text: err.Error()}}
		}
		if tc.Name == "task_wait" {
			kind = "background_wait_end"
			if isPermissionInterrupt(err) {
				kind = "background_wait_suspended"
			}
		}
		return m.sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: kind, ToolCallID: tc.CallID, ToolName: tc.Name, Status: status, Content: content})
	}
	var settleErr error
	if value, ok := m.grants.LoadAndDelete(tc); ok {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, settleErr = m.budget.ledger.Settle(cleanup, value.(durablebudget.Reservation), durablebudget.Settlement{Complete: true})
		cancel()
		if settleErr != nil {
			m.io.recordError(settleErr)
		}
	}
	status := "completed"
	var receipt *harness.ToolReceipt
	if err != nil {
		status = "failed"
		content = append(content, harness.Content{Type: "text", Text: err.Error()})
		receipt = &harness.ToolReceipt{Error: err.Error(), FailureCode: toolFailureCode(err), AgentRecoverable: recoverable && settleErr == nil}
		var notExecuted *harness.ToolNotExecutedError
		var noEffect *harness.ToolNoEffectError
		var executedFailure *harness.ToolExecutionFailedError
		if certifiedToolOutcome(err) {
			if errors.As(err, &notExecuted) {
				receipt.State = harness.ReceiptNotExecuted
			} else if errors.As(err, &noEffect) {
				receipt.State = harness.ReceiptNoEffect
			} else if errors.As(err, &executedFailure) {
				receipt.State = harness.ReceiptFailed
			}
		}
	}
	// Terminal cards and their durable events must survive cancellation of the
	// actual tool operation. The run still joins this callback before returning.
	return errors.Join(settleErr, m.sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: "tool_end", ToolCallID: tc.CallID, ToolName: tc.Name, Status: status, Content: content, Receipt: receipt}))
}

func (m *toolMiddleware) nativeDelegation(ctx context.Context, tc *adk.ToolContext) bool {
	return !unavailableTool(ctx, tc.Name) && (tc.Name == "task" || m.durableTaskWait && tc.Name == "task_wait")
}

func (m *toolMiddleware) requiresPermission(name string) bool {
	// A user answer is input, never a grant to execute another tool. Only the
	// host constructor's identity exempts this interaction from approval prompts.
	if m.userQuestion(name) {
		return false
	}
	if name == "skill_edit" || m.readOnly {
		return true
	}
	if protected, known := m.protected[name]; known {
		return protected
	}
	return (m.policy != nil || m.permissionMode != "") && m.permissionMode.RequiresToolPermission(name, m.trusted[name])
}

func (m *toolMiddleware) userQuestion(name string) bool {
	return name == "ask_user_question" && m.trusted[name]
}

func (m *toolMiddleware) askPermission(ctx context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
	done := beginRoleActivity(ctx, "approval")
	defer done()
	return m.permissions(ctx, request)
}

func (m *toolMiddleware) begin(ctx context.Context, tc *adk.ToolContext) (context.Context, func(), error) {
	if m.nativeDelegation(ctx, tc) {
		parent, _ := ctx.Value(roleWorkKey{}).(roleWork)
		identity, err := json.Marshal([]string{m.sink.request.RunID, parent.id, tc.CallID})
		if err != nil {
			return nil, nil, err
		}
		ctx = context.WithValue(ctx, roleInvocationKey{}, fmt.Sprintf("%x", sha256.Sum256(identity)))
		tracked, finish, err := m.io.begin(ctx)
		if err != nil {
			return nil, nil, err
		}
		done := beginRoleActivity(ctx, "descendant_wait")
		return tracked, func() { done(); finish() }, nil
	}
	tracked, finish, err := m.io.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	return tracked, func() {
		if release, ok := m.permits.LoadAndDelete(tc); ok {
			release.(func())()
		}
		finish()
	}, nil
}

// Operation failures belong to the agent, including unknown effects. Host
// cancellation, admission, persistence and interrupt errors remain control flow.
// This is called only for the tool endpoint's error, never a host callback error.
func recoverableToolFailure(ctx context.Context, err error) bool {
	// A tool's private context (or remote operation) may be cancelled while
	// the owning run is still active. Only the owning context and native
	// control markers establish user/run cancellation at this boundary.
	if err == nil || ctx.Err() != nil || errors.Is(err, adk.ErrStreamCanceled) || isPermissionInterrupt(err) {
		return false
	}
	var quota *budgetError
	var durableQuota *durablebudget.LimitError
	var persistence interface{ PersistenceFailure() bool }
	var nativeCancel *adk.CancelError
	var nativeInterrupt *adk.InterruptError
	return !errors.As(err, &quota) && !errors.As(err, &durableQuota) && !errors.As(err, &persistence) && !errors.As(err, &nativeCancel) && !errors.As(err, &nativeInterrupt) &&
		!errors.Is(err, durablebudget.ErrScope) && !errors.Is(err, durablebudget.ErrConflict) && !errors.Is(err, durablebudget.ErrUnknown) && !errors.Is(err, durablebudget.ErrClock) && !errors.Is(err, harness.ErrReceiptConflict)
}

func failedToolModelText(output string, err error) string {
	var executedFailure *harness.ToolExecutionFailedError
	if errors.As(err, &executedFailure) && certifiedToolOutcome(err) {
		return "Tool failed: " + err.Error() + "\nThe tool execution has ended. Partial effects may have occurred; inspect the output and any affected state before retrying. Do not assume a rollback.\nTool output: " + output
	}
	if !certifiedToolOutcome(err) {
		return "Tool failed: " + err.Error() + "\nExecution outcome and effects are unconfirmed. Partial effects may have occurred, and this result is not proof of success, failure without effects, or rollback. Inspect available evidence and affected state before deciding whether another attempt is appropriate.\nTool output: " + output
	}
	if output == "" {
		return "Tool failed: " + err.Error()
	}
	return "Tool failed: " + err.Error() + "\nTool output: " + output
}

func failedEnhancedToolResult(output *schema.ToolResult, err error) *schema.ToolResult {
	result := &schema.ToolResult{}
	if output != nil {
		*result = *output
		result.Parts = append([]schema.ToolOutputPart(nil), output.Parts...)
	}
	result.Parts = append(result.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeText, Text: failedToolModelText("", err)})
	return result
}

func (m *toolMiddleware) WrapInvokableToolCall(_ context.Context, next adk.InvokableToolCallEndpoint, tc *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		ctx, finish, err := m.begin(ctx, tc)
		if err != nil {
			return "", err
		}
		defer finish()
		if unavailableTool(ctx, tc.Name) {
			return m.rejectUnavailableTool(ctx, tc, args)
		}
		if prepare := m.preparers[tc.Name]; prepare != nil {
			args, err = prepare.PrepareToolArguments(ctx, args)
			if err != nil {
				// Invalid native arguments may contain secrets. Never persist or
				// request approval for the rejected raw payload.
				message := "Installed skill request rejected before execution; inspect the visible skill and submit bounded non-secret edits. " + safePreparationError(err)
				failure := harness.MarkToolNotExecuted(errors.New(message))
				var unavailable *harness.SkillAvailabilityError
				if errors.As(err, &unavailable) {
					failure = harness.MarkToolNotExecuted(harness.ClassifiedToolFailure(unavailable.Reason, message))
				}
				if e := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "pending", Arguments: json.RawMessage(`{}`)}); e != nil {
					return "", e
				}
				if e := m.finish(ctx, tc, nil, failure); e != nil {
					return "", e
				}
				return failedToolModelText("", failure), nil
			}
		}
		if err := m.start(ctx, tc, args); err != nil {
			if isPermissionInterrupt(err) {
				return "", err
			}
			if emitErr := m.finish(ctx, tc, nil, err); emitErr != nil {
				return "", errors.Join(err, emitErr)
			}
			if errors.Is(err, harness.ErrPermissionDenied) {
				return "Tool permission denied; no operation was performed.", nil
			}
			return "", err
		}
		var externalReservation *modelReservation
		var externalCommit func(context.Context) error
		if tc.Name == "invoke_acp_agent" {
			var err error
			externalReservation, _, err = m.budget.reserveContext(ctx, []*schema.Message{schema.UserMessage(args)}, nil)
			if err != nil {
				if emitErr := m.finish(ctx, tc, nil, err); emitErr != nil {
					return "", errors.Join(err, emitErr)
				}
				return "", err
			}
			ctx = acpclient.WithCallbacks(ctx, acpclient.Callbacks{
				ParentRunID:        m.sink.request.RunID,
				ParentToolCallID:   tc.CallID,
				ParentArgumentsSHA: fmt.Sprintf("%x", sha256.Sum256([]byte(args))),
				RegisterCompletion: func(complete func(context.Context) error) { externalCommit = complete },
				Usage: func(_ context.Context, usage acpclient.Usage) error {
					return externalReservation.observe(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
						PromptTokens: int(usage.InputTokens), CompletionTokens: int(usage.OutputTokens), TotalTokens: int(usage.TotalTokens),
					}}})
				},
				Update: func(_ context.Context, raw json.RawMessage) error {
					var update struct {
						SessionUpdate string          `json:"sessionUpdate"`
						Title         string          `json:"title"`
						Status        string          `json:"status"`
						Content       harness.Content `json:"content"`
					}
					if err := json.Unmarshal(raw, &update); err != nil {
						return err
					}
					var content []harness.Content
					switch update.SessionUpdate {
					case "agent_message_chunk":
						if update.Content.Type == "text" || update.Content.Type == "resource_link" {
							content = append(content, update.Content)
						}
						if update.Content.Type == "text" {
							if err := externalReservation.observe(&schema.Message{Content: update.Content.Text}); err != nil {
								return err
							}
						}
					case "tool_call", "tool_call_update":
						content = append(content, harness.Content{Type: "text", Text: "External tool " + update.Title + ": " + update.Status})
					}
					if len(content) == 0 {
						return nil
					}
					return m.sink.emit(ctx, harness.RunEvent{Kind: "tool_update", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress", Content: content})
				},
				Permission: func(_ context.Context, request acpclient.PermissionRequest) (string, error) {
					// Only the owning live connection can answer a remote request.
					// A lost connection stops the process; no stale option is replayed.
					if m.permissions == nil {
						return "", nil
					}
					var call struct {
						ToolCallID string          `json:"toolCallId"`
						Title      string          `json:"title"`
						RawInput   json.RawMessage `json:"rawInput"`
					}
					if err := json.Unmarshal(request.ToolCall, &call); err != nil {
						return "", err
					}
					if call.ToolCallID == "" || len(call.ToolCallID) > 256 || call.Title == "" || len(call.Title) > 128 || strings.ContainsAny(call.Title, "\r\n\x00") || len(call.RawInput) > 64<<10 {
						return "", errors.New("external permission has invalid tool identity or arguments")
					}
					var invoked struct {
						Agent string `json:"agent"`
					}
					if err := json.Unmarshal([]byte(args), &invoked); err != nil || invoked.Agent == "" {
						return "", errors.New("external permission has no agent identity")
					}
					arguments := call.RawInput
					if !json.Valid(arguments) {
						arguments = json.RawMessage(`{}`)
					}
					decision, err := m.askPermission(ctx, harness.PermissionRequest{ID: m.sink.request.RunID + "/" + tc.CallID + "/external/" + call.ToolCallID, SessionID: m.sink.request.Session.ID, RunID: m.sink.request.RunID, ConfigVersion: m.sink.request.Session.ConfigVersion, ToolCallID: tc.CallID + "/" + call.ToolCallID, ToolName: "external_acp/" + invoked.Agent + "/" + call.Title, Arguments: arguments})
					if err != nil {
						return "", err
					}
					for _, option := range request.Options {
						if option.Kind == string(decision) {
							return option.OptionID, nil
						}
					}
					return "", nil
				},
			})
		}
		if tc.Name == "skill_manage" {
			ctx = context.WithValue(ctx, skillReviewRuntimeKey{}, &skillReviewRuntime{budget: m.budget, io: m.io, sink: m.sink})
		}
		if tc.Name == "skill_edit" {
			ctx = context.WithValue(ctx, confirmedToolKey{}, confirmedTool{tc.Name, args})
		}
		output, toolErr := next(ctx, args, opts...)
		var settleErr error
		if externalReservation != nil {
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			usage, err := externalReservation.settleContext(settleCtx, toolErr == nil)
			settleErr = err
			if settleErr == nil {
				settleErr = m.sink.emit(settleCtx, harness.RunEvent{Kind: "usage", Usage: &usage})
			}
			cancel()
		}
		err = errors.Join(toolErr, settleErr)
		// Eino alpha discards a tool's returned value when it also returns an
		// error. Command failures carry their bounded execution evidence on the
		// error so durable receipts retain stdout, exit code and termination state.
		var evidence interface{ ToolResult() string }
		if output == "" && errors.As(err, &evidence) {
			output = evidence.ToolResult()
		}
		recoverable := settleErr == nil && m.recoverableFailure(ctx, tc, toolErr)
		modelOutput := output
		if recoverable {
			modelOutput = failedToolModelText(output, toolErr)
		}
		if (err == nil || recoverable) && tc.Name != "read_tool_output" {
			var snapshotErr error
			modelOutput, snapshotErr = m.modelToolOutput(ctx, modelOutput)
			err = errors.Join(err, snapshotErr)
			recoverable = recoverable && snapshotErr == nil
		}
		if tc.Name == "skill_manage" && m.io.failure() != nil {
			recoverable = false
		}
		if emitErr := m.finish(ctx, tc, []harness.Content{{Type: "text", Text: output}}, err, recoverable); emitErr != nil {
			return "", errors.Join(err, emitErr)
		}
		if err == nil && externalCommit != nil {
			commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			commitErr := externalCommit(commitCtx)
			cancel()
			if commitErr != nil {
				m.io.recordError(commitErr)
				return "", commitErr
			}
		}
		if tc.Name == "skill_manage" {
			// The candidate service can durably report a failed validation while
			// its nested model admission/settlement had an infrastructure error.
			// Keep the confirmed tool receipt, but stop before the next agent
			// call instead of swallowing that error as an ordinary review result.
			if nestedErr := m.io.failure(); nestedErr != nil {
				return "", errors.Join(err, nestedErr)
			}
		}
		if recoverable {
			return modelOutput, nil
		}
		return modelOutput, err
	}, nil
}

func (m *toolMiddleware) WrapStreamableToolCall(_ context.Context, next adk.StreamableToolCallEndpoint, tc *adk.ToolContext) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		ctx, finish, err := m.begin(ctx, tc)
		if err != nil {
			return nil, err
		}
		handedOff := false
		defer func() {
			if !handedOff {
				finish()
			}
		}()
		if err := m.start(ctx, tc, args); err != nil {
			if isPermissionInterrupt(err) {
				return nil, err
			}
			if emitErr := m.finish(ctx, tc, nil, err); emitErr != nil {
				return nil, errors.Join(err, emitErr)
			}
			if errors.Is(err, harness.ErrPermissionDenied) {
				return schema.StreamReaderFromArray([]string{"Tool permission denied; no operation was performed."}), nil
			}
			return nil, err
		}
		stream, err := next(ctx, args, opts...)
		if err != nil {
			var output string
			var evidence interface{ ToolResult() string }
			if errors.As(err, &evidence) {
				output = evidence.ToolResult()
			}
			recoverable := m.recoverableFailure(ctx, tc, err)
			modelOutput := ""
			if recoverable {
				var snapshotErr error
				modelOutput, snapshotErr = m.modelToolOutput(ctx, failedToolModelText(output, err))
				recoverable = snapshotErr == nil
				err = errors.Join(err, snapshotErr)
			}
			if finishErr := m.finish(ctx, tc, []harness.Content{{Type: "text", Text: output}}, err, recoverable); finishErr != nil {
				return nil, errors.Join(err, finishErr)
			}
			if recoverable {
				return schema.StreamReaderFromArray([]string{modelOutput}), nil
			}
			return nil, err
		}
		handedOff = true
		return relayToolStream(ctx, m, tc, stream, finish, func(chunk string) []harness.Content { return []harness.Content{{Type: "text", Text: chunk}} }, func(err error) (string, error) {
			return m.modelToolOutput(ctx, "\n"+failedToolModelText("", err))
		}), nil
	}, nil
}

func enhancedContent(output *schema.ToolResult) []harness.Content {
	if output == nil {
		return nil
	}
	var contents []harness.Content
	for _, part := range output.Parts {
		if part.Type == schema.ToolPartTypeText {
			contents = append(contents, harness.Content{Type: "text", Text: part.Text})
		}
		if part.Type == schema.ToolPartTypeImage && part.Image != nil {
			c := harness.Content{Type: "image", MimeType: part.Image.MIMEType}
			if part.Image.URL != nil {
				c.URI = *part.Image.URL
			}
			if ref, err := assetFromPart(part.Extra); err == nil {
				c.Asset, c.Name, c.Size = &ref, ref.Name, &ref.Size
			}
			contents = append(contents, c)
		}
		if part.Type == schema.ToolPartTypeFile && part.File != nil && part.File.URL != nil {
			c := harness.Content{Type: "resource_link", URI: *part.File.URL, MimeType: part.File.MIMEType}
			if ref, err := assetFromPart(part.Extra); err == nil {
				c.Asset, c.Name, c.Size = &ref, ref.Name, &ref.Size
			}
			contents = append(contents, c)
		}
	}
	return contents
}

func (m *toolMiddleware) WrapEnhancedInvokableToolCall(_ context.Context, next adk.EnhancedInvokableToolCallEndpoint, tc *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		ctx, finish, err := m.begin(ctx, tc)
		if err != nil {
			return nil, err
		}
		defer finish()
		if args == nil {
			return nil, errors.New("nil tool arguments")
		}
		if err := m.start(ctx, tc, args.Text); err != nil {
			if isPermissionInterrupt(err) {
				return nil, err
			}
			return nil, errors.Join(err, m.finish(ctx, tc, nil, err))
		}
		output, toolErr := next(ctx, args, opts...)
		if output == nil && toolErr != nil {
			var carrier interface{ EnhancedToolResult() *schema.ToolResult }
			if errors.As(toolErr, &carrier) {
				output = carrier.EnhancedToolResult()
			}
		}
		var mediaErr error
		output, mediaErr = normalizeToolImages(ctx, output, m.sink.request, tc.CallID, m.images)
		err = errors.Join(toolErr, mediaErr)
		fullContent := enhancedContent(output)
		modelOutput := output
		recoverable := mediaErr == nil && m.recoverableFailure(ctx, tc, toolErr)
		if recoverable {
			modelOutput = failedEnhancedToolResult(output, toolErr)
		}
		if err == nil || recoverable {
			var snapshotErr error
			modelOutput, snapshotErr = m.modelEnhancedToolOutput(ctx, modelOutput)
			err = errors.Join(err, snapshotErr)
			recoverable = recoverable && snapshotErr == nil
		}
		if emitErr := m.finish(ctx, tc, fullContent, err, recoverable); emitErr != nil {
			return nil, errors.Join(err, emitErr)
		}
		if recoverable {
			return modelOutput, nil
		}
		return modelOutput, err
	}, nil
}

func (m *toolMiddleware) WrapEnhancedStreamableToolCall(_ context.Context, next adk.EnhancedStreamableToolCallEndpoint, tc *adk.ToolContext) (adk.EnhancedStreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
		ctx, finish, err := m.begin(ctx, tc)
		if err != nil {
			return nil, err
		}
		handedOff := false
		defer func() {
			if !handedOff {
				finish()
			}
		}()
		if args == nil {
			return nil, errors.New("nil tool arguments")
		}
		if err := m.start(ctx, tc, args.Text); err != nil {
			if isPermissionInterrupt(err) {
				return nil, err
			}
			return nil, errors.Join(err, m.finish(ctx, tc, nil, err))
		}
		stream, err := next(ctx, args, opts...)
		if err != nil {
			var output *schema.ToolResult
			var evidence interface{ EnhancedToolResult() *schema.ToolResult }
			if errors.As(err, &evidence) {
				output = evidence.EnhancedToolResult()
			}
			var mediaErr error
			output, mediaErr = normalizeToolImages(ctx, output, m.sink.request, tc.CallID, m.images)
			recoverable := mediaErr == nil && m.recoverableFailure(ctx, tc, err)
			err = errors.Join(err, mediaErr)
			var modelOutput *schema.ToolResult
			if recoverable {
				var snapshotErr error
				modelOutput, snapshotErr = m.modelEnhancedToolOutput(ctx, failedEnhancedToolResult(output, err))
				recoverable = snapshotErr == nil
				err = errors.Join(err, snapshotErr)
			}
			if finishErr := m.finish(ctx, tc, enhancedContent(output), err, recoverable); finishErr != nil {
				return nil, errors.Join(err, finishErr)
			}
			if recoverable {
				return schema.StreamReaderFromArray([]*schema.ToolResult{modelOutput}), nil
			}
			return nil, err
		}
		handedOff = true
		return relayToolStream(ctx, m, tc, stream, finish, enhancedContent, func(err error) (*schema.ToolResult, error) {
			return m.modelEnhancedToolOutput(ctx, failedEnhancedToolResult(nil, err))
		}, func(result *schema.ToolResult) (*schema.ToolResult, error) {
			return normalizeToolImages(ctx, result, m.sink.request, tc.CallID, m.images)
		}), nil
	}, nil
}

// Complete the durable tool receipt before exposing terminal EOF to Eino.
// Errors after effects began, including errors while draining cleanup, remain
// failures even if earlier chunks looked successful.
func relayToolStream[T any](ctx context.Context, m *toolMiddleware, tc *adk.ToolContext, source *schema.StreamReader[T], release func(), content func(T) []harness.Content, failureResult func(error) (T, error), project ...func(T) (T, error)) *schema.StreamReader[T] {
	reader, writer := schema.Pipe[T](1)
	go func() {
		var terminal error
		var hostFailure bool
		var terminalImages []harness.Content
		defer func() {
			if source != nil {
				source.Close()
			}
			terminal = errors.Join(terminal, ctx.Err())
			recoverable := !hostFailure && m.recoverableFailure(ctx, tc, terminal)
			var modelFailure T
			if recoverable {
				var projectionErr error
				modelFailure, projectionErr = failureResult(terminal)
				terminal = errors.Join(terminal, projectionErr)
				recoverable = projectionErr == nil
			}
			if finishErr := m.finish(ctx, tc, terminalImages, terminal, recoverable); finishErr != nil {
				terminal = errors.Join(terminal, finishErr)
				recoverable = false
			}
			if recoverable {
				writer.Send(modelFailure, nil)
			} else if terminal != nil {
				m.io.recordError(terminal)
				var zero T
				writer.Send(zero, terminal)
			}
			release()
			writer.Close()
		}()
		if source == nil {
			terminal = errors.New("tool returned a nil stream")
			return
		}
		draining := false
		for {
			chunk, err := source.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				terminal = errors.Join(terminal, err)
				draining = true
				continue
			}
			if draining {
				continue
			}
			for _, transform := range project {
				chunk, err = transform(chunk)
				if err != nil {
					break
				}
			}
			if err != nil {
				terminal = errors.Join(terminal, err)
				hostFailure = true
				draining = true
				continue
			}
			projected := content(chunk)
			liveContent := make([]harness.Content, 0, len(projected))
			for _, item := range projected {
				if item.Type == "image" && item.Asset != nil {
					// The snapshot is not published until tool_end. Persisting its
					// reference in an update would break history after a crash.
					liveContent = append(liveContent, harness.Content{Type: "text", Text: "Image staged; available when this tool completes."})
				} else {
					liveContent = append(liveContent, item)
				}
			}
			if err = m.sink.emit(ctx, harness.RunEvent{Kind: "tool_update", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress", Content: liveContent}); err != nil {
				terminal = errors.Join(terminal, err)
				hostFailure = true
				draining = true
				continue
			}
			for _, item := range projected {
				if item.Type == "image" && item.Asset != nil {
					terminalImages = append(terminalImages, item)
				}
			}
			if writer.Send(chunk, nil) {
				terminal = errors.Join(terminal, context.Canceled)
				hostFailure = true
				draining = true
			}
		}
	}()
	return reader
}
