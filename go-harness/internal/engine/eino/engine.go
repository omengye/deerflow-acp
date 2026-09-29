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
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
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
	Cleanup  func() error
	State    json.RawMessage
}

var _ harness.Engine = (*Engine)(nil)

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
	chatModel := e.model
	if e.config.ChatModel == nil && req.Session.Model != "" && req.Session.Model != e.config.Model {
		var err error
		chatModel, err = newModel(ctx, e.config, req.Session.Model)
		if err != nil {
			return harness.RunResult{}, err
		}
	}
	tools := append([]tool.BaseTool(nil), e.config.Tools...)
	extensions := RunExtensions{}
	if e.config.ExtensionFactory != nil {
		var pinned json.RawMessage
		if savedCheckpoint != nil {
			pinned = append(json.RawMessage(nil), savedCheckpoint.Extension...)
		}
		var err error
		extensions, err = e.config.ExtensionFactory(ctx, req, pinned)
		if extensions.Cleanup != nil {
			defer func() { returnErr = errors.Join(returnErr, extensions.Cleanup()) }()
		}
		if err != nil {
			return harness.RunResult{}, fmt.Errorf("run extensions: %w", err)
		}
		if len(extensions.State) > 0 && !json.Valid(extensions.State) {
			return harness.RunResult{}, errors.New("extension state is not valid JSON")
		}
		tools = append(tools, extensions.Tools...)
	}
	if e.config.ToolFactory != nil {
		more, cleanup, err := e.config.ToolFactory(ctx, req)
		if cleanup != nil {
			// Registered before execution defers, so the pinned workspace stays
			// live through their cancellation, checkpoint and I/O cleanup.
			defer func() {
				if err := cleanup(); err != nil {
					returnErr = errors.Join(returnErr, fmt.Errorf("workspace tools cleanup: %w", err))
				}
			}()
		}
		if err != nil {
			return harness.RunResult{}, fmt.Errorf("workspace tools: %w", err)
		}
		tools = append(tools, more...)
	}
	protected := make(map[string]bool, len(tools))
	toolContracts := make([]*schema.ToolInfo, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			return harness.RunResult{}, errors.New("nil tool")
		}
		info, err := t.Info(ctx)
		if err != nil {
			return harness.RunResult{}, fmt.Errorf("tool info: %w", err)
		}
		if info == nil || info.Name == "" {
			return harness.RunResult{}, errors.New("tool name is required")
		}
		if protected[info.Name] {
			return harness.RunResult{}, fmt.Errorf("duplicate tool %q", info.Name)
		}
		protected[info.Name] = true
		toolContracts = append(toolContracts, info)
	}
	contract, err := e.executionContract(toolContracts, extensions.State)
	if err != nil {
		return harness.RunResult{}, err
	}
	// The native cancel controller cancels models/tools while the Runner's outer
	// context remains live long enough to commit cancellation and its checkpoint.
	execCtx, closeExecution := context.WithCancel(context.WithoutCancel(ctx))
	defer closeExecution()
	ioLifecycle := newRunIO(ctx)
	defer ioLifecycle.closeAndWait()
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
	mw := &toolMiddleware{sink: sink, permissions: permissions, protected: protected, io: ioLifecycle, budget: budget}
	selectedModel := req.Session.Model
	if selectedModel == "" {
		selectedModel = e.config.Model
	}
	media := &mediaProjection{resolver: e.config.AssetResolver, policy: e.config.Media, sessionID: req.Session.ID, model: selectedModel}
	handlers := append(append([]adk.ChatModelAgentMiddleware(nil), e.config.Handlers...), extensions.Handlers...)
	handlers = append(handlers, &modelLifecycle{io: ioLifecycle, budget: budget, sink: sink, media: media}, mw)
	agent, err := deep.New(execCtx, &deep.Config{
		Name: "deerflow", Description: "DeerFlow workspace assistant", Instruction: e.config.Instruction,
		ChatModel: chatModel, MaxIteration: e.config.MaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}},
		Handlers:    handlers, WithoutGeneralSubAgent: e.config.DisableSubAgent || (req.Session.ConfigVersion > 0 && !req.Session.Subagents),
	})
	if err != nil {
		return harness.RunResult{}, fmt.Errorf("create deep agent: %w", err)
	}
	result = harness.RunResult{StopReason: "end_turn"}
	var runErr error
	loop, checkpoints, err := e.turnLoop(ctx, req, resumeID, contract, extensions.State, budget, savedCheckpoint, agent, func(iter *adk.AsyncIterator[*adk.AgentEvent]) error {
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event == nil {
				continue
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
	runErr = errors.Join(runErr, withoutNativeTermination(exit.ExitReason), exit.CheckpointErr, checkpoints.failure())
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
	mu       sync.Mutex
	request  harness.RunRequest
	callback harness.EventHandler
	cancel   func()
	err      error
	active   map[string]string
	finished map[string]bool
}

func (s *eventSink) emit(ctx context.Context, event harness.RunEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
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
