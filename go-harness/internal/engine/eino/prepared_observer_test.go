package eino

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/backgroundtask"
	ds "github.com/cloudwego/eino/adk/backgroundtask/subagent"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

type observerRuntime struct{}

func (observerRuntime) Controls() <-chan backgroundtask.ControlRequest { return nil }
func (observerRuntime) EmitProgress(context.Context, string, []byte) (backgroundtask.ProgressEmission, error) {
	return backgroundtask.ProgressEmission{}, nil
}
func (observerRuntime) ReportTranscriptFailure(context.Context, error) error { return nil }

func observerSpec(t *testing.T, req harness.RunRequest, name string) backgroundtask.Spec {
	t.Helper()
	messages, err := (&schema.HumanReadableSerializer{}).Marshal([]*schema.Message{schema.UserMessage("test")})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"version": 4, "subagent_name": name, "child_session_id": req.Session.ID,
		"input": map[string]any{"messages": json.RawMessage(messages), "enable_streaming": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backgroundtask.Spec{ID: req.RunID, ExecutorKey: ds.ExecutorKey, Kind: "subagent", Payload: data}
}

func observerExecutor(t *testing.T, e *Engine, p *PreparedAttempt, hooks ExecutionHooks, name string, onInterrupt func(context.Context, []ExecutionInterruptBinding) error) *ds.Executor[*schema.Message] {
	t.Helper()
	executor, err := ds.NewExecutor(&ds.ExecutorConfig[*schema.Message]{SessionStore: e.config.SessionStore, CheckPointStore: e.config.CheckpointStore})
	if err != nil {
		t.Fatal(err)
	}
	agent := p.ObserveAgent(BindExecutionHooks(p.Agent, hooks), onInterrupt)
	if err = executor.Register(name, &ds.AgentRegistration[*schema.Message]{Agent: agent}); err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestPreparedObserverNativeBackgroundJSONPermissionResume(t *testing.T) {
	for _, decision := range []harness.PermissionDecision{harness.AllowOnce, harness.RejectOnce} {
		t.Run(string(decision), func(t *testing.T) {
			limits := harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 1, MaxTokens: 100000, MaxOutputTokens: 50}
			store, ledger, req, scope := durableFixture(t, "observed-background-"+string(decision), limits)
			req.Session.ConfigVersion = 7
			underlying := &recordingTool{}
			fake := toolScript()
			e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger, SessionStore: store, CheckpointStore: store})
			broker := &fixtureInteractionBroker{decisions: make(map[string]harness.PermissionDecision)}
			hooks := ExecutionHooks{Broker: broker, StageCheckpoint: func(context.Context, StagedExecutionCheckpoint) error {
				t.Error("foreground checkpoint hook called")
				return nil
			}}
			var events []harness.RunEvent
			var eventMu sync.Mutex
			emit := func(_ context.Context, event harness.RunEvent) error {
				eventMu.Lock()
				defer eventMu.Unlock()
				events = append(events, event)
				return nil
			}
			ctx := durablebudget.WithScope(context.Background(), scope)
			p, err := e.PrepareAttempt(ctx, req, "worker-v1", nil, emit, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var bindings []ExecutionInterruptBinding
			executor := observerExecutor(t, e, p, hooks, "worker-v1", func(_ context.Context, observed []ExecutionInterruptBinding) error { bindings = observed; return nil })
			task := &backgroundtask.Task{Spec: observerSpec(t, req, "worker-v1"), Attempt: 1, Status: backgroundtask.StatusRunning}
			result, err := executor.Execute(ctx, task, observerRuntime{})
			joinErr := p.JoinAndClose(context.Background())
			if err != nil || joinErr != nil || result == nil || result.Status != backgroundtask.StatusWaitingInput || len(bindings) != 1 || len(result.Checkpoint) == 0 {
				t.Fatalf("native background pause: result=%+v err=%v join=%v bindings=%v", result, err, joinErr, bindings)
			}
			if err = ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeWaitingInput); err != nil {
				t.Fatal(err)
			}
			binding := bindings[0]
			broker.decisions[binding.IntentID] = decision
			hooks.Targets = map[string]PermissionResume{binding.NativeInterruptID: {IntentID: binding.IntentID, IntentVersion: binding.IntentVersion, GrantID: "grant/" + binding.IntentID}}
			task.Checkpoint = result.Checkpoint
			task.PendingResume, err = json.Marshal(hooks.Targets)
			if err != nil {
				t.Fatal(err)
			}
			task.Attempt = 2
			scope.AttemptID, scope.Fence = req.RunID+"/2", 2
			if err = ledger.BeginAttempt(context.Background(), scope); err != nil {
				t.Fatal(err)
			}
			ctx = durablebudget.WithScope(context.Background(), scope)
			p, err = e.PrepareAttempt(ctx, req, "worker-v1", nil, emit, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			executor = observerExecutor(t, e, p, hooks, "worker-v1", func(context.Context, []ExecutionInterruptBinding) error {
				t.Error("approved resume interrupted again")
				return nil
			})
			result, err = executor.Execute(ctx, task, observerRuntime{})
			joinErr = p.JoinAndClose(context.Background())
			if err != nil || joinErr != nil || result == nil || result.Status != backgroundtask.StatusCompleted {
				t.Fatalf("native JSON resume: result=%+v err=%v join=%v", result, err, joinErr)
			}
			wantCalls := int32(0)
			if decision == harness.AllowOnce {
				wantCalls = 1
			}
			if underlying.calls.Load() != wantCalls || broker.prepared != 1 || broker.resolved != 1 {
				t.Fatalf("authorization/effect count: tools=%d prepared=%d resolved=%d", underlying.calls.Load(), broker.prepared, broker.resolved)
			}
			snapshot, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
			if err != nil || snapshot.ToolCalls != 1 || snapshot.ModelCalls != 2 || snapshot.HeldTokens != 0 {
				t.Fatalf("native JSON accounting: %+v %v", snapshot, err)
			}
			eventMu.Lock()
			defer eventMu.Unlock()
			counts := make(map[string]int)
			for _, event := range events {
				counts[event.Kind]++
			}
			if counts["tool_start"] != 1 || counts["tool_end"] != 1 || counts["tool_execute"] != int(wantCalls) || counts["usage"] != 2 || counts["text_delta"] != 1 {
				t.Fatalf("observed duplicate/missing domain events: %v", counts)
			}
		})
	}
}

func TestPreparedObserverNativeBackgroundTextAndLateStreamError(t *testing.T) {
	want := errors.New("late provider failure")
	var stopped = make(chan struct{})
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		reader, writer := schema.Pipe[*schema.Message](1)
		go func() {
			defer writer.Close()
			defer close(stopped)
			writer.Send(&schema.Message{Role: schema.Assistant, Content: "prefix", ReasoningContent: "think"}, nil)
			writer.Send(nil, want)
			time.Sleep(20 * time.Millisecond)
		}()
		return reader, nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	var mu sync.Mutex
	var text, reasoning string
	p, err := e.PrepareAttempt(context.Background(), request("late-stream"), "worker-v1", nil, func(_ context.Context, event harness.RunEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if event.Kind == "text_delta" {
			text += event.Text
		}
		if event.Kind == "reasoning_delta" {
			reasoning += event.Text
		}
		return nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := observerExecutor(t, e, p, ExecutionHooks{}, "worker-v1", nil)
	_, err = executor.Execute(context.Background(), &backgroundtask.Task{Spec: observerSpec(t, request("late-stream"), "worker-v1"), Attempt: 1}, observerRuntime{})
	joinErr := p.JoinAndClose(context.Background())
	if !errors.Is(err, want) || !errors.Is(joinErr, want) {
		t.Fatalf("late error lost: execute=%v join=%v", err, joinErr)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("join returned before provider stopped")
	}
	mu.Lock()
	defer mu.Unlock()
	if text != "prefix" || reasoning != "think" {
		t.Fatalf("projection text=%q reasoning=%q", text, reasoning)
	}
}

func TestPreparedObserverConstructionCancellationReachesNativeInvocation(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	fake := &scriptedModel{stream: func(ctx context.Context, _ int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := e.PrepareAttempt(ctx, request("construction-cancel"), "worker-v1", nil, nil, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	executor := observerExecutor(t, e, p, ExecutionHooks{}, "worker-v1", nil)
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(context.Background(), &backgroundtask.Task{Spec: observerSpec(t, request("construction-cancel"), "worker-v1"), Attempt: 1}, observerRuntime{})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("construction cancel did not reach provider")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("native background did not finish")
	}
	if err = p.JoinAndClose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedObserverProjectionFailureCancelsAndPreservesError(t *testing.T) {
	want := errors.New("event persistence failed")
	fake := &scriptedModel{stream: func(ctx context.Context, _ int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		reader, writer := schema.Pipe[*schema.Message](1)
		go func() {
			defer writer.Close()
			writer.Send(schema.AssistantMessage("stop", nil), nil)
			<-ctx.Done()
		}()
		return reader, nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	p, err := e.PrepareAttempt(context.Background(), request("projection-failure"), "worker-v1", nil, func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "text_delta" {
			return want
		}
		return nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := observerExecutor(t, e, p, ExecutionHooks{}, "worker-v1", nil)
	_, err = executor.Execute(context.Background(), &backgroundtask.Task{Spec: observerSpec(t, request("projection-failure"), "worker-v1"), Attempt: 1}, observerRuntime{})
	joinErr := p.JoinAndClose(context.Background())
	if !errors.Is(err, want) || !errors.Is(joinErr, want) {
		t.Fatalf("projection failure lost: execute=%v join=%v", err, joinErr)
	}
	executionErr, cleanupErr := p.JoinAndCloseDetailed(context.Background())
	if !errors.Is(executionErr, want) || cleanupErr != nil {
		t.Fatalf("joined projection failure classified as cleanup uncertainty: %v / %v", executionErr, cleanupErr)
	}
}

func TestPreparedObserverInterruptStagingFailureIsNotWaitingInput(t *testing.T) {
	want := errors.New("interrupt staging failed")
	broker := &fixtureInteractionBroker{}
	e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{&recordingTool{}}})
	p, err := e.PrepareAttempt(context.Background(), request("interrupt-failure"), "worker-v1", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hooks := ExecutionHooks{Broker: broker, StageCheckpoint: func(context.Context, StagedExecutionCheckpoint) error { return nil }}
	executor := observerExecutor(t, e, p, hooks, "worker-v1", func(context.Context, []ExecutionInterruptBinding) error { return want })
	result, err := executor.Execute(context.Background(), &backgroundtask.Task{Spec: observerSpec(t, request("interrupt-failure"), "worker-v1"), Attempt: 1}, observerRuntime{})
	joinErr := p.JoinAndClose(context.Background())
	if !errors.Is(err, want) || !errors.Is(joinErr, want) || (result != nil && result.Status == backgroundtask.StatusWaitingInput) {
		t.Fatalf("staging failure became success: result=%+v execute=%v join=%v", result, err, joinErr)
	}
}

type silentObserverAgent struct{}

func (silentObserverAgent) Name(context.Context) string        { return "silent" }
func (silentObserverAgent) Description(context.Context) string { return "test agent" }
func (silentObserverAgent) Run(context.Context, *adk.AgentInput, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, writer := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	writer.Close()
	return iter
}
func (a silentObserverAgent) Resume(ctx context.Context, _ *adk.ResumeInfo, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.Run(ctx, nil, opts...)
}

func TestPreparedObserverSuppressedBudgetFailureIsNativeErrorNotCleanup(t *testing.T) {
	e := newTestEngine(t, Config{ChatModel: toolScript()})
	p, err := e.PrepareAttempt(context.Background(), request("suppressed-limit"), "worker-v1", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.budget.mu.Lock()
	p.budget.fail("model_calls")
	p.budget.mu.Unlock()
	iter := p.ObserveAgent(silentObserverAgent{}, nil).Run(context.Background(), nil)
	event, ok := iter.Next()
	var limit *durablebudget.LimitError
	if !ok || !errors.As(event.Err, &limit) || limit.Resource != "model_calls" {
		t.Fatalf("suppressed budget failure was lost: %+v", event)
	}
	if _, ok = iter.Next(); ok {
		t.Fatal("unexpected additional event")
	}
	if err = p.JoinAndClose(context.Background()); err != nil {
		t.Fatalf("known budget limit contaminated cleanup: %v", err)
	}
}
