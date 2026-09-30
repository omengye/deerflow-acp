package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	harnesstools "github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

// PreparedAttempt is one fresh assembly of the same model/tool/media/budget
// stack used by foreground TurnLoop execution. A background host owns its
// native runner, persistence and attempt lease; this value owns only the agent
// and its real I/O/resources. Do not reuse it across attempts.
type PreparedAttempt struct {
	Agent        adk.ResumableAgent
	Contract     string
	State        json.RawMessage
	ctx          context.Context
	budget       *runBudget
	io           *runIO
	sink         *eventSink
	cleanups     []func() error
	resourceOnce sync.Once
	resourceErr  error
	joinOnce     sync.Once
	executionErr error
	cleanupErr   error
	postRun      func(context.Context, string, bool) error
}

// PrepareAttempt requires the already-active trusted budget scope when a
// ledger is configured. Even if err is non-nil, a non-nil returned attempt must
// be joined. name is the immutable host-registered agent version. cancel must
// stop the caller's native runner without skipping provider/process cleanup.
func (e *Engine) PrepareAttempt(ctx context.Context, req harness.RunRequest, name string, pinned json.RawMessage, events harness.EventHandler, permissions harness.PermissionHandler, cancel func()) (*PreparedAttempt, error) {
	if req.Session.ID == "" || req.RunID == "" || name == "" {
		return nil, errors.New("session, run and agent names are required")
	}
	b, err := e.newBudget(ctx, req)
	if err != nil {
		return nil, err
	}
	sink := &eventSink{request: req, callback: events, cancel: cancel, active: make(map[string]string), finished: make(map[string]bool)}
	return e.prepareAgent(ctx, req, name, pinned, b, sink, newRunIO(ctx), permissions)
}

func (p *PreparedAttempt) closeResources() error {
	p.resourceOnce.Do(func() {
		for i := len(p.cleanups) - 1; i >= 0; i-- {
			p.resourceErr = errors.Join(p.resourceErr, p.cleanups[i]())
		}
	})
	return p.resourceErr
}

// JoinAndClose deliberately has no early deadline return: timeout does not
// prove that a provider stream or child process has actually stopped.
func (p *PreparedAttempt) JoinAndClose(ctx context.Context) error {
	executionErr, cleanupErr := p.JoinAndCloseDetailed(ctx)
	return errors.Join(executionErr, cleanupErr)
}

// JoinAndCloseDetailed separates errors observed from already joined execution
// from resource teardown uncertainty. Both are failures, but only the latter
// means a host must retain shared resources because cleanup is unconfirmed.
// Receipt/ledger persistence still independently gates every task transition.
func (p *PreparedAttempt) JoinAndCloseDetailed(ctx context.Context) (executionErr, cleanupErr error) {
	p.joinOnce.Do(func() {
		p.io.closeAndWait()
		p.executionErr = errors.Join(p.io.failure(), p.sink.closeOpen(context.WithoutCancel(ctx)), p.sink.failure())
		p.cleanupErr = p.closeResources()
	})
	return p.executionErr, p.cleanupErr
}

func (p *PreparedAttempt) RemainingTime() (time.Duration, bool) {
	return p.budget.remainingTime(), p.budget.limits.Timeout > 0
}
func (p *PreparedAttempt) BudgetFailure() error {
	if err := p.budget.failure(); err != nil {
		return &durablebudget.LimitError{Resource: err.resource}
	}
	return nil
}

func (e *Engine) prepareAgent(ctx context.Context, req harness.RunRequest, name string, pinned json.RawMessage, b *runBudget, sink *eventSink, ioLifecycle *runIO, permissions harness.PermissionHandler) (*PreparedAttempt, error) {
	p := &PreparedAttempt{ctx: ctx, budget: b, io: ioLifecycle, sink: sink}
	chatModel := e.model
	if e.config.ChatModel == nil && req.Session.Model != "" && req.Session.Model != e.config.Model {
		var err error
		selected := e.config
		modelID := req.Session.Model
		if e.config.ModelRoutes != nil {
			route, ok := e.config.ModelRoutes[req.Session.Model]
			if !ok {
				return p, fmt.Errorf("%w: session model is not configured", harness.ErrInvalidInput)
			}
			selected.Provider, selected.BaseURL, selected.APIKey = route.Provider, route.BaseURL, route.APIKey
			modelID = route.Model
		}
		chatModel, err = newModel(ctx, selected, modelID)
		if err != nil {
			return p, err
		}
	}
	tools := append([]tool.BaseTool(nil), e.config.Tools...)
	extensions := RunExtensions{}
	if e.config.ExtensionFactory != nil {
		var err error
		extensions, err = e.config.ExtensionFactory(ctx, req, append(json.RawMessage(nil), pinned...))
		if extensions.Cleanup != nil {
			p.cleanups = append(p.cleanups, extensions.Cleanup)
		}
		if err != nil {
			return p, fmt.Errorf("run extensions: %w", err)
		}
		if len(extensions.State) > 0 && !json.Valid(extensions.State) {
			return p, errors.New("extension state is not valid JSON")
		}
		tools = append(tools, extensions.Tools...)
	}
	p.State = append(json.RawMessage(nil), extensions.State...)
	if e.config.ToolFactory != nil {
		more, cleanup, err := e.config.ToolFactory(ctx, req)
		if cleanup != nil {
			p.cleanups = append(p.cleanups, func() error {
				if err := cleanup(); err != nil {
					return fmt.Errorf("workspace tools cleanup: %w", err)
				}
				return nil
			})
		}
		if err != nil {
			return p, fmt.Errorf("workspace tools: %w", err)
		}
		tools = append(tools, more...)
	}
	if e.config.ToolOutputStore != nil {
		readOutput, err := harnesstools.ReadToolOutputTool(e.config.ToolOutputStore, req.Session)
		if err != nil {
			return p, fmt.Errorf("tool output reader: %w", err)
		}
		tools = append(tools, readOutput)
	}
	protected := make(map[string]bool, len(tools))
	seenTools := make(map[string]bool, len(tools))
	contracts := make([]*schema.ToolInfo, 0, len(tools))
	allowedTools := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			return p, errors.New("nil tool")
		}
		info, err := t.Info(ctx)
		if err != nil {
			return p, fmt.Errorf("tool info: %w", err)
		}
		if info == nil || info.Name == "" {
			return p, errors.New("tool name is required")
		}
		if !e.config.ToolPolicy.Allows(info.Name) {
			continue
		}
		if info.Name == "task" && !e.config.DisableSubAgent && !(req.Session.ConfigVersion > 0 && !req.Session.Subagents) {
			return p, errors.New("tool name task is reserved for native Eino delegation")
		}
		if info.Name == "write_todos" {
			return p, errors.New("tool name write_todos is reserved for native Eino planning")
		}
		if seenTools[info.Name] {
			return p, fmt.Errorf("duplicate tool %q", info.Name)
		}
		seenTools[info.Name] = true
		protected[info.Name] = e.config.PermissionMode.RequiresPermission(info.Name) || req.Session.Mode == "plan" || req.Session.ApprovalMode == harness.ApprovalReadOnly
		contracts = append(contracts, info)
		allowedTools = append(allowedTools, t)
	}
	tools = allowedTools
	// DeepAgent injects these native tools after ToolsNodeConfig is assembled.
	// Govern them through the same middleware when the host policy requires it.
	for _, name := range []string{"task", "write_todos", "skill"} {
		if e.config.ToolPolicy.Allows(name) && e.config.PermissionMode != "" && e.config.PermissionMode.RequiresPermission(name) {
			protected[name] = true
		}
	}
	var err error
	p.Contract, err = e.executionContract(contracts, extensions.State)
	if err != nil {
		return p, err
	}
	selectedModel := req.Session.Model
	if selectedModel == "" {
		selectedModel = e.config.Model
	}
	media := &mediaProjection{resolver: e.config.AssetResolver, importer: e.config.ModelImageImporter, request: req, policy: e.config.Media, sessionID: req.Session.ID, model: selectedModel,
		publish: func(ctx context.Context, content harness.Content) error {
			return sink.emit(ctx, harness.RunEvent{Kind: "image_delta", Content: []harness.Content{content}})
		}}
	mw := &toolMiddleware{sink: sink, permissions: permissions, protected: protected, io: ioLifecycle, budget: b, images: e.config.ToolImageImporter, outputs: e.config.ToolOutputStore}
	handlers := append(append([]adk.ChatModelAgentMiddleware(nil), e.config.Handlers...), extensions.Handlers...)
	if extensions.ModelHandlerFactory != nil || extensions.PostRunFactory != nil {
		privateMedia := *media
		privateMedia.importer, privateMedia.publish = nil, nil
		metered := &trackedModel{inner: chatModel, io: ioLifecycle, budget: b, sink: sink, media: &privateMedia}
		if extensions.ModelHandlerFactory != nil {
			modelHandlers, err := extensions.ModelHandlerFactory(ctx, metered)
			if err != nil {
				return p, fmt.Errorf("build tracked model middleware: %w", err)
			}
			handlers = append(handlers, modelHandlers...)
		}
		if extensions.PostRunFactory != nil {
			var err error
			p.postRun, err = extensions.PostRunFactory(ctx, metered)
			if err != nil {
				return p, fmt.Errorf("build tracked post-run model: %w", err)
			}
		}
	}
	if e.config.Compaction.Enabled {
		privateMedia := *media
		privateMedia.importer, privateMedia.publish = nil, nil
		metered := &trackedModel{inner: chatModel, io: ioLifecycle, budget: b, sink: sink, media: &privateMedia}
		compaction, err := newCompactionMiddleware(ctx, e.config.Compaction, metered)
		if err != nil {
			return p, fmt.Errorf("build context compaction: %w", err)
		}
		handlers = append(handlers, compaction)
	}
	handlers = append(handlers, &modelLifecycle{io: ioLifecycle, budget: b, sink: sink, media: media}, mw)
	p.Agent, err = deep.New(ctx, &deep.Config{
		Name: name, Description: "DeerFlow workspace assistant", Instruction: e.config.Instruction + extensions.InstructionAppend,
		ChatModel: chatModel, MaxIteration: e.config.MaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}},
		Handlers:    handlers, WithoutWriteTodos: !e.config.ToolPolicy.Allows("write_todos"),
		WithoutGeneralSubAgent: e.config.DisableSubAgent || !e.config.ToolPolicy.Allows("task") || (req.Session.ConfigVersion > 0 && !req.Session.Subagents),
	})
	if err != nil {
		return p, fmt.Errorf("create deep agent: %w", err)
	}
	return p, nil
}
