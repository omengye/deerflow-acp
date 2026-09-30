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
)

type toolMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	sink        *eventSink
	permissions harness.PermissionHandler
	protected   map[string]bool
	io          *runIO
	budget      *runBudget
	images      harness.ToolImageImporter
	outputs     harness.ToolOutputStore
	grants      sync.Map // *adk.ToolContext -> durablebudget.Reservation
	outputMu    sync.Mutex
	outputBytes int
}

func (m *toolMiddleware) start(ctx context.Context, tc *adk.ToolContext, args string) error {
	arguments := json.RawMessage(args)
	if !json.Valid(arguments) {
		return errors.New("tool arguments are not valid JSON")
	}
	if m.nativeDelegation(tc) {
		// Eino's task endpoint is a resumable orchestration boundary. Its child
		// models and actual tools own the effect admissions and receipts. Giving
		// this boundary a started effect receipt would make every safe child
		// permission suspension look like an unjoined external side effect.
		// The native task tool owns its interrupt state as well. A permission
		// interrupt here would be mistaken for its own child interrupt, so the
		// boundary uses the live session broker; child effects remain durable.
		was, _, _ := tool.GetInterruptState[any](ctx)
		if m.protected[tc.Name] && !was {
			// Persist a terminal receipt for the orchestration boundary before
			// entering the child. The child may suspend; an open parent receipt
			// would otherwise be classified as an uncertain external effect.
			if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "pending", Arguments: append(json.RawMessage(nil), arguments...)}); err != nil {
				return err
			}
			decision := harness.RejectOnce
			var err error
			if m.permissions != nil {
				decision, err = m.permissions(ctx, m.requestFor(tc, arguments))
			}
			if err != nil || (decision != harness.AllowOnce && decision != harness.AllowAlways) {
				failure := harness.ErrPermissionDenied
				if err != nil {
					failure = err
				} else if decision == harness.PermissionCancelled {
					failure = context.Canceled
				}
				if endErr := m.sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: "tool_end", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "failed", Content: []harness.Content{{Type: "text", Text: failure.Error()}}, Receipt: &harness.ToolReceipt{Error: failure.Error()}}); endErr != nil {
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
		return m.sink.emit(ctx, harness.RunEvent{Kind: kind, ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress"})
	}
	hooks := executionHooks(ctx)
	governed := m.protected[tc.Name] && hooks != nil && hooks.Broker != nil
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
	if m.protected[tc.Name] {
		var decision harness.PermissionDecision
		var err error
		if governed {
			if hooks.StageCheckpoint == nil {
				return errors.New("durable permission broker requires checkpoint staging")
			}
			decision, err = m.governedPermission(ctx, tc, args, hooks)
		} else {
			if m.permissions == nil {
				return harness.ErrPermissionDenied
			}
			decision, err = m.permissions(ctx, m.requestFor(tc, arguments))
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
	if value, ok := m.grants.Load(tc); ok {
		first, err := m.budget.ledger.MarkDispatched(ctx, value.(durablebudget.Reservation))
		if err != nil {
			return m.budget.classify(err)
		}
		if !first {
			return durablebudget.ErrConflict
		}
	}
	return m.sink.emit(ctx, harness.RunEvent{Kind: "tool_execute", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress"})
}

func (m *toolMiddleware) finish(ctx context.Context, tc *adk.ToolContext, content []harness.Content, err error) error {
	if m.nativeDelegation(tc) {
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
		receipt = &harness.ToolReceipt{Error: err.Error()}
	}
	// Terminal cards and their durable events must survive cancellation of the
	// actual tool operation. The run still joins this callback before returning.
	return errors.Join(settleErr, m.sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: "tool_end", ToolCallID: tc.CallID, ToolName: tc.Name, Status: status, Content: content, Receipt: receipt}))
}

func (m *toolMiddleware) nativeDelegation(tc *adk.ToolContext) bool {
	return tc.Name == "task"
}

func (m *toolMiddleware) WrapInvokableToolCall(_ context.Context, next adk.InvokableToolCallEndpoint, tc *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		ctx, finish, err := m.io.begin(ctx)
		if err != nil {
			return "", err
		}
		defer finish()
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
					decision, err := m.permissions(ctx, harness.PermissionRequest{ID: m.sink.request.RunID + "/" + tc.CallID + "/external/" + call.ToolCallID, SessionID: m.sink.request.Session.ID, RunID: m.sink.request.RunID, ConfigVersion: m.sink.request.Session.ConfigVersion, ToolCallID: tc.CallID + "/" + call.ToolCallID, ToolName: "external_acp/" + invoked.Agent + "/" + call.Title, Arguments: arguments})
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
		output, err := next(ctx, args, opts...)
		if externalReservation != nil {
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			usage, settleErr := externalReservation.settleContext(settleCtx, err == nil)
			if settleErr == nil {
				settleErr = m.sink.emit(settleCtx, harness.RunEvent{Kind: "usage", Usage: &usage})
			}
			cancel()
			err = errors.Join(err, settleErr)
		}
		// Eino alpha discards a tool's returned value when it also returns an
		// error. Command failures carry their bounded execution evidence on the
		// error so durable receipts retain stdout, exit code and termination state.
		var evidence interface{ ToolResult() string }
		if output == "" && errors.As(err, &evidence) {
			output = evidence.ToolResult()
		}
		modelOutput := output
		if err == nil && tc.Name != "read_tool_output" {
			var snapshotErr error
			modelOutput, snapshotErr = m.modelToolOutput(ctx, output)
			err = errors.Join(err, snapshotErr)
		}
		if emitErr := m.finish(ctx, tc, []harness.Content{{Type: "text", Text: output}}, err); emitErr != nil {
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
		return modelOutput, err
	}, nil
}

func (m *toolMiddleware) WrapStreamableToolCall(_ context.Context, next adk.StreamableToolCallEndpoint, tc *adk.ToolContext) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		ctx, finish, err := m.io.begin(ctx)
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
			return nil, errors.Join(err, m.finish(ctx, tc, nil, err))
		}
		handedOff = true
		return relayToolStream(ctx, m, tc, stream, finish, func(chunk string) []harness.Content { return []harness.Content{{Type: "text", Text: chunk}} }), nil
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
		ctx, finish, err := m.io.begin(ctx)
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
		output, err := next(ctx, args, opts...)
		var mediaErr error
		output, mediaErr = normalizeToolImages(ctx, output, m.sink.request, tc.CallID, m.images)
		err = errors.Join(err, mediaErr)
		fullContent := enhancedContent(output)
		modelOutput := output
		if err == nil {
			var snapshotErr error
			modelOutput, snapshotErr = m.modelEnhancedToolOutput(ctx, output)
			err = errors.Join(err, snapshotErr)
		}
		if emitErr := m.finish(ctx, tc, fullContent, err); emitErr != nil {
			return nil, errors.Join(err, emitErr)
		}
		return modelOutput, err
	}, nil
}

func (m *toolMiddleware) WrapEnhancedStreamableToolCall(_ context.Context, next adk.EnhancedStreamableToolCallEndpoint, tc *adk.ToolContext) (adk.EnhancedStreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
		ctx, finish, err := m.io.begin(ctx)
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
			return nil, errors.Join(err, m.finish(ctx, tc, nil, err))
		}
		handedOff = true
		return relayToolStream(ctx, m, tc, stream, finish, enhancedContent, func(result *schema.ToolResult) (*schema.ToolResult, error) {
			return normalizeToolImages(ctx, result, m.sink.request, tc.CallID, m.images)
		}), nil
	}, nil
}

// Complete the durable tool receipt before exposing terminal EOF to Eino.
// Errors after effects began, including errors while draining cleanup, remain
// failures even if earlier chunks looked successful.
func relayToolStream[T any](ctx context.Context, m *toolMiddleware, tc *adk.ToolContext, source *schema.StreamReader[T], release func(), content func(T) []harness.Content, project ...func(T) (T, error)) *schema.StreamReader[T] {
	reader, writer := schema.Pipe[T](1)
	go func() {
		var terminal error
		var terminalImages []harness.Content
		defer func() {
			if source != nil {
				source.Close()
			}
			terminal = errors.Join(terminal, ctx.Err())
			if finishErr := m.finish(ctx, tc, terminalImages, terminal); finishErr != nil {
				terminal = errors.Join(terminal, finishErr)
			}
			m.io.recordError(terminal)
			release()
			writer.Close()
		}()
		if source == nil {
			terminal = errors.New("tool returned a nil stream")
			var zero T
			writer.Send(zero, terminal)
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
				if !draining {
					var zero T
					writer.Send(zero, err)
					draining = true
				}
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
				var zero T
				writer.Send(zero, err)
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
				writer.Send(chunk, err)
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
				draining = true
			}
		}
	}()
	return reader
}
