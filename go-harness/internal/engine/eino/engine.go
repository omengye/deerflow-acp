// Package eino adapts Eino's native DeepAgent and managed Runner to the harness.
package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/cloudwego/eino/adk"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// Config is internal to the Eino adapter. Business and transport packages use
// harness.Engine, so alpha API changes stay at this boundary.
type Config struct {
	Provider, APIKey, BaseURL, Model, Instruction string
	ChatModel                                     model.ToolCallingChatModel
	Tools                                         []tool.BaseTool
	// ToolFactory opens resources owned by this run, such as a pinned os.Root.
	// Cleanup runs after native execution and all tracked I/O have drained, even
	// when construction or execution fails. Static Tools remain caller-owned.
	ToolFactory func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error)
	// ExtensionFactory pins one snapshot for both tools and middleware. On
	// explicit resume, pinned is the state saved in the checkpoint envelope.
	ExtensionFactory func(context.Context, harness.RunRequest, json.RawMessage) (RunExtensions, error)
	CheckpointStore  adk.CheckPointStore
	SessionStore     adk.SessionEventStore[*schema.Message]
	MaxIterations    int
	Budget           harness.BudgetLimits
	BudgetLedger     *durablebudget.Ledger
	Media            harness.MediaConfig
	AssetResolver    harness.AssetResolver
	// Handlers extends native Eino middleware without introducing another loop.
	Handlers        []adk.ChatModelAgentMiddleware
	DisableSubAgent bool
}

type Engine struct {
	config Config
	model  model.ToolCallingChatModel
}

// RunExtensions is constructed exactly once for a run. State must describe the
// immutable resources used by both Tools and Handlers; it enters the execution
// contract and checkpoint. Cleanup runs after all tracked execution has joined.
type RunExtensions struct {
	Tools    []tool.BaseTool
	Handlers []adk.ChatModelAgentMiddleware
	// ModelHandlerFactory builds middleware that makes its own model calls,
	// such as summarization or memory extraction. The supplied model is tracked
	// by the same I/O lifecycle and durable budget as the main agent. It must
	// be used instead of an unwrapped provider model for those calls.
	ModelHandlerFactory func(context.Context, model.BaseModel[*schema.Message]) ([]adk.ChatModelAgentMiddleware, error)
	Cleanup             func() error
	State               json.RawMessage
}

var _ harness.Engine = (*Engine)(nil)

// DurableExecutions lets the neutral runtime enable its persistent execution
// lifecycle only when the engine has authoritative shared budget storage.
func (e *Engine) DurableExecutions() bool { return e.config.BudgetLedger != nil }

func New(ctx context.Context, config Config) (*Engine, error) {
	if err := validateBudget(config.Budget); err != nil {
		return nil, err
	}
	if config.MaxIterations == 0 {
		config.MaxIterations = 50
	}
	if config.MaxIterations < 1 {
		return nil, errors.New("max iterations must be positive")
	}
	if config.SessionStore == nil {
		config.SessionStore = einosession.NewInMemoryStore[*schema.Message](nil)
	}
	if config.CheckpointStore == nil {
		config.CheckpointStore = &memoryCheckpoints{items: make(map[string][]byte)}
	}
	config.Tools = append([]tool.BaseTool(nil), config.Tools...)
	config.Handlers = append([]adk.ChatModelAgentMiddleware(nil), config.Handlers...)
	config.Media.VisionModels = append([]string(nil), config.Media.VisionModels...)
	chatModel := config.ChatModel
	if chatModel == nil {
		var err error
		chatModel, err = newModel(ctx, config, config.Model)
		if err != nil {
			return nil, err
		}
	}
	return &Engine{config: config, model: chatModel}, nil
}

// CheckpointID identifies a run's native execution checkpoint. Ordinary next
// prompts start a new turn; resuming an interrupted execution is explicit.
func CheckpointID(runID string) string { return "harness/turn/v1/" + runID }

func (e *Engine) Run(ctx context.Context, req harness.RunRequest, events harness.EventHandler, permissions harness.PermissionHandler) (harness.RunResult, error) {
	return e.execute(ctx, req, "", events, permissions)
}

// Resume continues a known Eino checkpoint; req.Input is not silently appended
// to suspended tool execution. The caller must bind checkpointID to its session.
func (e *Engine) Resume(ctx context.Context, req harness.RunRequest, checkpointID string, events harness.EventHandler, permissions harness.PermissionHandler) (harness.RunResult, error) {
	if checkpointID == "" {
		return harness.RunResult{}, errors.New("checkpoint ID is required")
	}
	return e.execute(ctx, req, checkpointID, events, permissions)
}

func (e *Engine) execute(ctx context.Context, req harness.RunRequest, resumeID string, events harness.EventHandler, permissions harness.PermissionHandler) (result harness.RunResult, returnErr error) {
	if req.Session.ID == "" || req.RunID == "" {
		return harness.RunResult{}, errors.New("session ID and run ID are required")
	}
	source, err := executionInputSource(ctx, req)
	if err != nil {
		return harness.RunResult{}, err
	}
	budget, err := e.newBudget(ctx, req)
	if err != nil {
		return harness.RunResult{}, err
	}
	var pendingCheckpoints *checkedCheckpoints
	// Registered first, so checkpoint mutations commit after every execution,
	// I/O, resource-cleanup and terminal budget-event defer has succeeded.
	defer func() {
		if pendingCheckpoints != nil && returnErr == nil {
			if err := pendingCheckpoints.commit(context.WithoutCancel(ctx), result.StopReason != "cancelled"); err != nil {
				returnErr = fmt.Errorf("commit execution checkpoint: %w", err)
			}
		}
	}()
	if e.config.Budget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, budget.remainingTime(), &budgetError{resource: "time"})
		defer cancel()
	}
	sink := &eventSink{request: req, callback: events, active: make(map[string]string), finished: make(map[string]bool)}
	defer func() {
		limit := budget.failure()
		if limit == nil {
			_ = errors.As(context.Cause(ctx), &limit)
		}
		if limit == nil {
			var shared *durablebudget.LimitError
			if errors.As(context.Cause(ctx), &shared) && !shared.Temporary {
				limit = &budgetError{resource: shared.Resource}
			}
		}
		if limit == nil {
			return
		}
		budget.mu.Lock()
		budget.fail(limit.resource)
		budget.mu.Unlock()
		result.StopReason, result.Limit = limit.stopReason(), limit.resource
		returnErr = withoutBudgetTermination(returnErr)
		if err := sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: "budget_exhausted", Text: limit.Error()}); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	var savedCheckpoint *checkpointEnvelope
	if resumeID != "" {
		var err error
		savedCheckpoint, err = loadCheckpointEnvelope(ctx, e.config.CheckpointStore, resumeID)
		if err != nil {
			return harness.RunResult{}, err
		}
		if !sameExecutionSource(source, savedCheckpoint.InputSource) {
			return harness.RunResult{}, fmt.Errorf("%w: checkpoint input source changed", harness.ErrInvalidInput)
		}
		if budget.ledger != nil {
			if savedCheckpoint.Ledger == nil {
				return harness.RunResult{}, fmt.Errorf("%w: a local checkpoint cannot authorize durable quota", harness.ErrInvalidInput)
			}
			identity, identityErr := budget.durableIdentity(ctx)
			if identityErr != nil {
				return harness.RunResult{}, identityErr
			}
			if identity.RootBudgetID != savedCheckpoint.Ledger.RootBudgetID || identity.PolicyHash != savedCheckpoint.Ledger.PolicyHash || identity.Revision < savedCheckpoint.Ledger.Revision {
				return harness.RunResult{}, durablebudget.ErrConflict
			}
		} else {
			if savedCheckpoint.Budget == nil {
				return harness.RunResult{}, fmt.Errorf("%w: durable checkpoint requires its ledger", harness.ErrInvalidInput)
			}
			if err := budget.restore(*savedCheckpoint.Budget); err != nil {
				return harness.RunResult{}, err
			}
		}
		if e.config.Budget.Timeout > 0 {
			// Loading is bounded by this invocation's deadline; after restoration
			// tighten it to the unspent time of the complete logical execution.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeoutCause(ctx, budget.remainingTime(), &budgetError{resource: "time"})
			defer cancel()
		}
	}
	if err := ctx.Err(); err != nil {
		return harness.RunResult{StopReason: "cancelled"}, nil
	}
	// The native cancel controller cancels models/tools while the Runner's outer
	// context remains live long enough to commit cancellation and its checkpoint.
	execCtx, closeExecution := context.WithCancel(context.WithoutCancel(ctx))
	defer closeExecution()
	ioLifecycle := newRunIO(ctx)
	// Assigned before starting the loop or any model/tool work.
	stopLoop := func() {}
	requestCancel := func() {
		stopLoop()
		// TurnLoop dispatches native cancellation asynchronously. Cancelling the
		// provider first can turn this into an ordinary model error before Eino
		// captures its checkpoint. The native loop stops first; closeAndWait
		// below then cancels and joins any remaining provider/tool cleanup.
	}
	sink.cancel = requestCancel
	var pinned json.RawMessage
	if savedCheckpoint != nil {
		pinned = savedCheckpoint.Extension
	} else if source != nil {
		pinned = executionHooks(ctx).InputSource.PinnedExtension
	}
	prepared, err := e.prepareAgent(ctx, req, "deerflow", pinned, budget, sink, ioLifecycle, permissions)
	if prepared != nil {
		defer func() { returnErr = errors.Join(returnErr, prepared.closeResources()) }()
	}
	defer ioLifecycle.closeAndWait()
	if err != nil {
		return harness.RunResult{}, err
	}
	result = harness.RunResult{StopReason: "end_turn"}
	var runErr error
	var bindings []ExecutionInterruptBinding
	captureInterrupt := func(contexts []*adk.InterruptCtx) error {
		hooks := executionHooks(ctx)
		if hooks == nil || hooks.Broker == nil || hooks.StageCheckpoint == nil {
			return errors.New("execution interrupt requires trusted runtime hooks")
		}
		captured, err := collectInterruptBindings(contexts)
		if err != nil {
			return err
		}
		if len(bindings) > 0 {
			old, _ := json.Marshal(bindings)
			fresh, _ := json.Marshal(captured)
			if string(old) != string(fresh) {
				return errors.New("native interruption targets changed during suspension")
			}
		}
		bindings = captured
		result.StopReason = "waiting_input"
		return nil
	}
	loop, checkpoints, err := e.turnLoop(ctx, req, resumeID, prepared.Contract, prepared.State, budget, savedCheckpoint, prepared.Agent, func(iter *adk.AsyncIterator[*adk.AgentEvent]) error {
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event == nil {
				continue
			}
			if event.Action != nil && event.Action.Interrupted != nil {
				if err := captureInterrupt(event.Action.Interrupted.InterruptContexts); err != nil {
					runErr = errors.Join(runErr, err)
				}
			}
			if event.Err != nil {
				var cancelled *adk.CancelError
				switch {
				case errors.As(event.Err, &cancelled), errors.Is(event.Err, context.Canceled), errors.Is(event.Err, adk.ErrStreamCanceled):
					result.StopReason = "cancelled"
				case errors.Is(event.Err, adk.ErrExceedMaxIterations):
					result.StopReason = "max_turn_requests"
				case errors.Is(event.Err, adk.ErrSessionBusy):
					runErr = errors.Join(runErr, harness.ErrBusy)
				}
				runErr = errors.Join(runErr, withoutNativeTermination(event.Err))
			}
			if event.Output != nil && event.Output.MessageOutput != nil {
				if err := consumeMessage(execCtx, event.Output.MessageOutput, sink, &result); err != nil {
					runErr = errors.Join(runErr, withoutNativeTermination(err))
					requestCancel()
				}
			}
		}
		return runErr
	})
	if err != nil {
		return harness.RunResult{}, err
	}
	pendingCheckpoints = checkpoints
	stopLoop = func() { loop.Stop(adk.WithImmediate()) }
	done := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-ctx.Done():
			requestCancel()
		case <-done:
		}
	}()
	defer func() { close(done); <-monitorDone }()
	loop.Run(execCtx)
	exit := loop.Wait()
	exitErr := exit.ExitReason
	var interrupt *adk.InterruptError
	if errors.As(exitErr, &interrupt) {
		if err := captureInterrupt(interrupt.InterruptContexts); err != nil {
			runErr = errors.Join(runErr, err)
		} else {
			exitErr, _ = removeErrorLeaves(exitErr, func(leaf error) bool { _, ok := leaf.(*adk.InterruptError); return ok })
		}
	}
	checkpoints.setInterrupts(bindings)
	runErr = errors.Join(runErr, withoutNativeTermination(exitErr), exit.CheckpointErr, checkpoints.failure())
	ioLifecycle.closeAndWait()
	runErr = errors.Join(runErr, ioLifecycle.failure())
	if err := sink.closeOpen(execCtx); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if err := sink.failure(); err != nil {
		return result, errors.Join(runErr, err)
	}
	if ctx.Err() != nil {
		result.StopReason = "cancelled"
	}
	return result, runErr
}

func withoutNativeTermination(err error) error {
	remaining, _ := removeErrorLeaves(err, func(leaf error) bool {
		_, cancelled := leaf.(*adk.CancelError)
		return cancelled || leaf == context.Canceled || leaf == adk.ErrStreamCanceled || leaf == adk.ErrExceedMaxIterations
	})
	return remaining
}

func (e *Engine) input(ctx context.Context, req harness.RunRequest) ([]*schema.Message, error) {
	if hooks := executionHooks(ctx); hooks != nil && hooks.InputSource != nil {
		return notificationInput(ctx, req)
	}
	for _, message := range append(append([]harness.Message(nil), req.History...), harness.Message{Content: req.Input}) {
		for _, part := range message.Content {
			if part.Asset != nil && part.Asset.SessionID != req.Session.ID {
				return nil, fmt.Errorf("%w: asset belongs to another session", harness.ErrPermissionDenied)
			}
		}
	}
	stored, err := e.config.SessionStore.LoadEvents(ctx, req.Session.ID, &adk.LoadSessionEventsRequest{Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	var messages []*schema.Message
	// Runner reconstructs its own complete tool history. Business history is only
	// a bootstrap for an empty native session, never appended a second time.
	if stored == nil || len(stored.Events) == 0 {
		for _, historical := range req.History {
			if historical.Role != "user" && historical.Role != "assistant" {
				return nil, fmt.Errorf("unsupported history role %q", historical.Role)
			}
			m, err := convertContent(schema.RoleType(historical.Role), historical.Content)
			if err != nil {
				return nil, err
			}
			messages = append(messages, m)
		}
	}
	message, err := convertContent(schema.User, req.Input)
	if err != nil {
		return nil, err
	}
	return append(messages, message), nil
}

func consumeMessage(ctx context.Context, variant *adk.MessageVariant, sink *eventSink, result *harness.RunResult) error {
	var toolCallID string
	consume := func(msg *schema.Message) error {
		if msg == nil {
			return nil
		}
		if (variant.Role == schema.Tool || msg.Role == schema.Tool) && msg.ToolCallID != "" {
			toolCallID = msg.ToolCallID
		}
		if variant.Role == schema.Assistant || (variant.Role == "" && msg.Role == schema.Assistant) {
			if msg.Content != "" {
				if err := sink.emit(ctx, harness.RunEvent{Kind: "text_delta", Text: msg.Content}); err != nil {
					return err
				}
			}
			if msg.ReasoningContent != "" {
				if err := sink.emit(ctx, harness.RunEvent{Kind: "reasoning_delta", Text: msg.ReasoningContent}); err != nil {
					return err
				}
			}
			if meta := msg.ResponseMeta; meta != nil {
				if meta.FinishReason == "length" {
					result.StopReason = "max_tokens"
				}
				if meta.FinishReason == "content_filter" {
					result.StopReason = "refusal"
				}
			}
		}
		return nil
	}
	if variant.IsStreaming && variant.MessageStream != nil {
		defer variant.MessageStream.Close()
		for {
			msg, err := variant.MessageStream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if err := consume(msg); err != nil {
				return err
			}
		}
	} else if err := consume(variant.Message); err != nil {
		return err
	}
	if toolCallID != "" {
		if err := sink.emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: toolCallID, ToolName: variant.ToolName, Status: "completed"}); err != nil {
			return err
		}
	}
	return nil
}

// eventSink serializes concurrent tools and remembers transport failures. The
// caller continues draining the native iterator so cleanup finishes before Run.
type eventSink struct {
	mu          sync.Mutex
	request     harness.RunRequest
	callback    harness.EventHandler
	cancel      func()
	err         error
	active      map[string]string
	finished    map[string]bool
	delegations map[string]bool
}

func (s *eventSink) emit(ctx context.Context, event harness.RunEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if event.Kind == "tool_end" && s.delegations[event.ToolCallID] && (event.ToolName == "task" || (event.ToolName == "" && s.active[event.ToolCallID] == "")) {
		return nil
	}
	if event.Kind == "tool_start" {
		s.active[event.ToolCallID] = event.ToolName
	}
	if event.Kind == "tool_end" {
		if s.finished[event.ToolCallID] {
			return nil
		}
		delete(s.active, event.ToolCallID)
		s.finished[event.ToolCallID] = true
	}
	event.SessionID, event.RunID = s.request.Session.ID, s.request.RunID
	if s.callback != nil {
		if err := s.callback(ctx, event); err != nil {
			s.err = err
			if s.cancel != nil {
				s.cancel()
			}
			return err
		}
	}
	return nil
}
func (s *eventSink) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

func (s *eventSink) closeOpen(ctx context.Context) error {
	s.mu.Lock()
	remaining := make(map[string]string, len(s.active))
	for id, name := range s.active {
		remaining[id] = name
	}
	s.mu.Unlock()
	for id, name := range remaining {
		if err := s.emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: id, ToolName: name, Status: "failed", Content: []harness.Content{{Type: "text", Text: "Tool execution ended before completion."}}}); err != nil {
			return err
		}
	}
	return nil
}

type memoryCheckpoints struct {
	mu    sync.Mutex
	items map[string][]byte
}

func (s *memoryCheckpoints) Get(_ context.Context, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	return append([]byte(nil), p...), ok, nil
}
func (s *memoryCheckpoints) Set(_ context.Context, id string, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = append([]byte(nil), p...)
	return nil
}
func (s *memoryCheckpoints) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}
