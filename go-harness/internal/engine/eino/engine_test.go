package eino

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type scriptedModel struct {
	mu     sync.Mutex
	calls  int
	seen   [][]*schema.Message
	stream func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error)
}

func (m *scriptedModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}
func (m *scriptedModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	s, err := m.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var messages []*schema.Message
	for {
		message, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return schema.ConcatMessages(messages)
}
func (m *scriptedModel) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	call := m.calls
	m.calls++
	m.seen = append(m.seen, append([]*schema.Message(nil), input...))
	m.mu.Unlock()
	return m.stream(ctx, call, input)
}

func request(id string) harness.RunRequest {
	return harness.RunRequest{Session: harness.Session{ID: "test-session", Mode: "default"}, RunID: id, InputID: "input-" + id, Input: []harness.Content{{Type: "text", Text: "hello"}}}
}
func newTestEngine(t *testing.T, config Config) *Engine {
	t.Helper()
	config.DisableSubAgent = true
	e, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func textStream(text string) *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: text}})
}

func TestContextUsageUsesConfiguredWindowAndLastLeadCall(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "first", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}, ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}}}}), nil
		}
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "done", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 30, CompletionTokens: 3, TotalTokens: 33}}}}), nil
	}}
	// The model may return tool calls before its final answer. Only the last
	// lead call describes the next turn's approximate context occupancy.
	e := newTestEngine(t, Config{ChatModel: fake, Model: "fixture", Tools: []tool.BaseTool{&recordingTool{}}, ContextWindows: map[string]int{"fixture": 4096}})
	input := request("context-usage")
	input.Session.Model = "fixture"
	var events []harness.RunEvent
	result, err := e.Run(context.Background(), input, func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err == nil && result.StopReason == "end_turn" {
		var contextEvents []harness.ContextUsage
		for _, event := range events {
			if event.ContextUsage != nil {
				contextEvents = append(contextEvents, *event.ContextUsage)
			}
		}
		if len(contextEvents) != 1 || contextEvents[0].Size != 4096 || contextEvents[0].Used != 33 {
			t.Fatalf("context updates=%+v events=%+v", contextEvents, events)
		}
		return
	}
	t.Fatalf("result=%+v err=%v", result, err)
}

func TestContextUsageExcludesNativeSubagentCalls(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"general-purpose","prompt":"inspect","description":"inspect"}`}}}, ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 2}}}}), nil
		case 1:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "child answer", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 400, CompletionTokens: 50}}}}), nil
		default:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "parent answer", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 30, CompletionTokens: 3}}}}), nil
		}
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Model: "fixture", ContextWindows: map[string]int{"fixture": 4096}})
	if err != nil {
		t.Fatal(err)
	}
	input := request("context-subagent")
	input.Session.Model = "fixture"
	var contextEvents []harness.ContextUsage
	result, err := e.Run(context.Background(), input, func(_ context.Context, event harness.RunEvent) error {
		if event.ContextUsage != nil {
			contextEvents = append(contextEvents, *event.ContextUsage)
		}
		return nil
	}, nil)
	if err != nil || result.StopReason != "end_turn" || len(contextEvents) != 1 || contextEvents[0].Used != 33 {
		t.Fatalf("result=%+v err=%v context=%+v", result, err, contextEvents)
	}
}

func TestContextUsageSkipsMissingLastLeadUsage(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "first", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}, ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 2}}}}), nil
		}
		return textStream("done without usage"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Model: "fixture", Tools: []tool.BaseTool{&recordingTool{}}, ContextWindows: map[string]int{"fixture": 4096}})
	input := request("context-last-missing")
	input.Session.Model = "fixture"
	var contextEvents int
	result, err := e.Run(context.Background(), input, func(_ context.Context, event harness.RunEvent) error {
		if event.ContextUsage != nil {
			contextEvents++
		}
		return nil
	}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.StopReason != "end_turn" || contextEvents != 0 {
		t.Fatalf("result=%+v err=%v context updates=%d", result, err, contextEvents)
	}
}

func TestNativeRunnerStreamsAndReconstructsSession(t *testing.T) {
	dir := t.TempDir()
	store, err := einosession.NewFileStore[*schema.Message](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := &scriptedModel{stream: func(_ context.Context, _ int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray([]*schema.Message{
			{Role: schema.Assistant, Content: "hel"}, {Role: schema.Assistant, Content: "lo", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}}},
		}), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, SessionStore: store})
	var events []harness.RunEvent
	result, err := e.Run(context.Background(), request("one"), func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, nil)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var text string
	var usage *harness.Usage
	for _, event := range events {
		if event.Kind == "text_delta" {
			text += event.Text
		}
		if event.Kind == "usage" {
			usage = event.Usage
		}
	}
	if text != "hello" || usage == nil || usage.TotalTokens != 10 {
		t.Fatalf("text=%q usage=%+v", text, usage)
	}
	// A new adapter and store instance must recover the complete native history.
	reopened, err := einosession.NewFileStore[*schema.Message](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	e = newTestEngine(t, Config{ChatModel: fake, SessionStore: reopened})
	second := request("two")
	second.Input[0].Text = "second"
	second.History = []harness.Message{{Role: "user", Content: []harness.Content{{Type: "text", Text: "MUST_NOT_DUPLICATE"}}}}
	if _, err := e.Run(context.Background(), second, nil, nil); err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, msg := range fake.seen[1] {
		if msg.Role == schema.User {
			users = append(users, msg.Content)
		}
	}
	if strings.Join(users, "|") != "hello|second" {
		t.Fatalf("native history=%q", users)
	}
}

type recordingTool struct{ calls atomic.Int32 }

func (*recordingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "read_workspace", Desc: "read a workspace value", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"value": {Type: schema.String}})}, nil
}
func (t *recordingTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	t.calls.Add(1)
	return "tool result: " + args, nil
}

func toolScript() *scriptedModel {
	return &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"x"}`}}}}}), nil
		}
		return textStream("done"), nil
	}}
}

func TestNativeToolPermissionAndEvents(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "allow", false: "deny"}[allow], func(t *testing.T) {
			underlying := &recordingTool{}
			e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{underlying}})
			var events []harness.RunEvent
			var approvals int
			result, err := e.Run(context.Background(), request("tools"), func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, func(_ context.Context, permission harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				if underlying.calls.Load() != 0 {
					t.Error("tool executed before permission")
				}
				if permission.ToolCallID != "call-1" || string(permission.Arguments) != `{"value":"x"}` {
					t.Errorf("wrong permission: %+v", permission)
				}
				if allow {
					return harness.AllowOnce, nil
				}
				return harness.RejectOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if approvals != 1 {
				t.Fatalf("approvals=%d", approvals)
			}
			wantCalls := int32(0)
			if allow {
				wantCalls = 1
			}
			if underlying.calls.Load() != wantCalls {
				t.Fatalf("tool executions=%d", underlying.calls.Load())
			}
			var starts, ends int
			for _, event := range events {
				if event.Kind == "tool_start" {
					starts++
					if event.ToolCallID != "call-1" {
						t.Error("tool identity lost")
					}
				}
				if event.Kind == "tool_end" {
					ends++
					if allow && event.Status != "completed" || !allow && event.Status != "failed" {
						t.Errorf("end status=%q", event.Status)
					}
				}
			}
			if starts != 1 || ends != 1 {
				t.Fatalf("starts=%d ends=%d events=%+v", starts, ends, events)
			}
		})
	}
}

func TestGeneralSubagentInheritsToolPermissions(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "allow", false: "deny"}[allow], func(t *testing.T) {
			underlying := &recordingTool{}
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				switch call {
				case 0:
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate-1", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"general-purpose","prompt":"read the value","description":"read workspace value"}`}}}}}), nil
				case 1:
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "child-call-1", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"child"}`}}}}}), nil
				default:
					return textStream("done"), nil
				}
			}}
			e, err := New(context.Background(), Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}})
			if err != nil {
				t.Fatal(err)
			}
			var approvals int
			result, err := e.Run(context.Background(), request("delegate"), nil, func(_ context.Context, permission harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				if permission.SessionID != "test-session" || permission.RunID != "delegate" || permission.ToolCallID != "child-call-1" {
					t.Errorf("child lost parent authorization scope: %+v", permission)
				}
				if allow {
					return harness.AllowOnce, nil
				}
				return harness.RejectOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			wantCalls := int32(0)
			if allow {
				wantCalls = 1
			}
			if approvals != 1 || underlying.calls.Load() != wantCalls {
				t.Fatalf("approvals=%d executions=%d", approvals, underlying.calls.Load())
			}
		})
	}
}

type streamingTool struct{}

func (streamingTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&recordingTool{}).Info(ctx)
}
func (streamingTool) StreamableRun(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
	return schema.StreamReaderFromArray([]string{"one", "two"}), nil
}

func TestStreamingToolClosesCardExactlyOnce(t *testing.T) {
	e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{streamingTool{}}})
	var output strings.Builder
	var ends int
	_, err := e.Run(context.Background(), request("stream-tool"), func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "tool_update" {
			for _, part := range event.Content {
				output.WriteString(part.Text)
			}
		}
		if event.Kind == "tool_end" {
			ends++
			if event.Status != "completed" {
				t.Errorf("tool ended with %q", event.Status)
			}
		}
		return nil
	}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || output.String() != "onetwo" || ends != 1 {
		t.Fatalf("output=%q ends=%d err=%v", output.String(), ends, err)
	}
}

// diskCheckpoints is a minimal durable fixture. Production uses the injected
// SQLite provider; this test verifies Eino's real serialized resume boundary.
type diskCheckpoints struct{ dir string }

func (s *diskCheckpoints) path(id string) string {
	return filepath.Join(s.dir, strings.ReplaceAll(id, "/", "_")+".checkpoint")
}
func (s *diskCheckpoints) Get(_ context.Context, id string) ([]byte, bool, error) {
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}
func (s *diskCheckpoints) Set(_ context.Context, id string, data []byte) error {
	return os.WriteFile(s.path(id), data, 0600)
}
func (s *diskCheckpoints) Delete(_ context.Context, id string) error {
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func TestNativeCancellationSavesCheckpointAndResumes(t *testing.T) {
	started, modelStopped := make(chan struct{}), make(chan struct{})
	fake := &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call > 0 {
			return textStream("resumed"), nil
		}
		reader, writer := schema.Pipe[*schema.Message](1)
		go func() {
			defer writer.Close()
			defer close(modelStopped)
			close(started)
			<-ctx.Done()
			writer.Send(nil, ctx.Err())
		}()
		return reader, nil
	}}
	checkpoints := &diskCheckpoints{dir: t.TempDir()}
	sessions, err := einosession.NewFileStore[*schema.Message](t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{ChatModel: fake, CheckpointStore: checkpoints, SessionStore: sessions}
	e := newTestEngine(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		result, err := e.Run(ctx, request("cancel"), nil, nil)
		if err == nil && result.StopReason != "cancelled" {
			err = errors.New("wrong cancellation stop reason")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	select {
	case <-modelStopped:
	default:
		t.Fatal("Run returned before model teardown")
	}
	data, ok, err := checkpoints.Get(context.Background(), CheckpointID("cancel"))
	if err != nil || !ok || len(data) == 0 {
		t.Fatalf("checkpoint exists=%v bytes=%d err=%v", ok, len(data), err)
	}
	e = newTestEngine(t, config)
	result, err := e.Resume(context.Background(), request("resume"), CheckpointID("cancel"), nil, nil)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("resume result=%+v err=%v", result, err)
	}
}

func TestEventHandlerFailureStopsNativeRunner(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, _ int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("output"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	transportErr := errors.New("client connection closed")
	_, err := e.Run(context.Background(), request("disconnect"), func(context.Context, harness.RunEvent) error { return transportErr }, nil)
	if !errors.Is(err, transportErr) {
		t.Fatalf("got %v", err)
	}
}

type blockingTool struct{ started, stopped chan struct{} }

func (*blockingTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&recordingTool{}).Info(ctx)
}
func (b *blockingTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	close(b.started)
	<-ctx.Done()
	// Simulate cooperative resource cleanup after cancellation (for example a
	// subprocess Wait or closing a response body) before relinquishing ownership.
	time.Sleep(20 * time.Millisecond)
	close(b.stopped)
	return "", ctx.Err()
}

func TestCancellationJoinsToolCleanupAndPersistsTerminalCard(t *testing.T) {
	underlying := &blockingTool{started: make(chan struct{}), stopped: make(chan struct{})}
	e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{underlying}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var endings atomic.Int32
	done := make(chan error, 1)
	go func() {
		result, err := e.Run(ctx, request("cancel-tool"), func(eventCtx context.Context, event harness.RunEvent) error {
			if event.Kind == "tool_end" {
				endings.Add(1)
				if eventCtx.Err() != nil {
					return errors.New("terminal card persistence received canceled context")
				}
			}
			return nil
		}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.AllowOnce, nil
		})
		if err == nil && result.StopReason != "cancelled" {
			err = errors.New("wrong stop reason")
		}
		done <- err
	}()
	select {
	case <-underlying.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tool cleanup did not finish")
	}
	select {
	case <-underlying.stopped:
	default:
		t.Fatal("Run returned before tool resource cleanup")
	}
	if endings.Load() != 1 {
		t.Fatalf("terminal cards=%d", endings.Load())
	}
}

func TestToolFactoryCleanupWaitsForCanceledTool(t *testing.T) {
	underlying := &blockingTool{started: make(chan struct{}), stopped: make(chan struct{})}
	var cleanups atomic.Int32
	e := newTestEngine(t, Config{ChatModel: toolScript(), ToolFactory: func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
		return []tool.BaseTool{underlying}, func() error {
			cleanups.Add(1)
			select {
			case <-underlying.stopped:
				return nil
			default:
				return errors.New("workspace closed before tool cleanup")
			}
		}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Run(ctx, request("factory-cancel"), nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.AllowOnce, nil
		})
		done <- err
	}()
	select {
	case <-underlying.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	if cleanups.Load() != 0 {
		t.Fatal("workspace closed while tool was running")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run cleanup did not finish")
	}
	if cleanups.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleanups.Load())
	}
}

func TestToolFactoryCleanupPreservesBothErrors(t *testing.T) {
	constructionErr := errors.New("factory could not create all tools")
	cleanupErr := errors.New("workspace close failed")
	var cleanups int
	e := newTestEngine(t, Config{ChatModel: toolScript(), ToolFactory: func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
		return nil, func() error { cleanups++; return cleanupErr }, constructionErr
	}})
	_, err := e.Run(context.Background(), request("factory-error"), nil, nil)
	if !errors.Is(err, constructionErr) || !errors.Is(err, cleanupErr) || cleanups != 1 {
		t.Fatalf("err=%v cleanup count=%d", err, cleanups)
	}
}

func TestToolFactoryCleanupFailureAfterSuccessfulRun(t *testing.T) {
	cleanupErr := errors.New("workspace close failed")
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("done"), nil
	}}
	var cleanups int
	e := newTestEngine(t, Config{ChatModel: fake, ToolFactory: func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
		return nil, func() error { cleanups++; return cleanupErr }, nil
	}})
	_, err := e.Run(context.Background(), request("cleanup-error"), nil, nil)
	if !errors.Is(err, cleanupErr) || cleanups != 1 {
		t.Fatalf("err=%v cleanup count=%d", err, cleanups)
	}
}

func TestProviderConstructorsCompileAgainstPinnedEino(t *testing.T) {
	for _, provider := range []string{"openai", "claude", "ark"} {
		t.Run(provider, func(t *testing.T) {
			if _, err := New(context.Background(), Config{Provider: provider, Model: "test-model", APIKey: "test-key"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImageInputPreservesPartsAndRejectsMismatchedData(t *testing.T) {
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aZ1sAAAAASUVORK5CYII="
	ref, _ := testImageAsset(t)
	input := []harness.Content{{Type: "text", Text: "inspect "}, {Type: "image", Asset: &ref, URI: harness.AssetURI(ref), MimeType: "image/png"}, {Type: "text", Text: " this"}}
	msg, err := convertContent(schema.User, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.UserInputMultiContent) != 3 || msg.Content != "" || msg.UserInputMultiContent[1].Image.Base64Data != nil || *msg.UserInputMultiContent[1].Image.URL != harness.AssetURI(ref) {
		t.Fatalf("bad image mapping: %+v", msg)
	}
	for _, bad := range []harness.Content{
		{Type: "image", Data: "not base64", MimeType: "image/png"},
		{Type: "image", Data: png, MimeType: "image/jpeg"},
		{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("plain text")), MimeType: "image/png"},
		{Type: "image", URI: "file:///secret.png", MimeType: "image/png"},
	} {
		if _, err := convertContent(schema.User, []harness.Content{bad}); err == nil {
			t.Errorf("accepted invalid image %+v", bad)
		}
	}
}

var _ adk.CheckPointStore = (*diskCheckpoints)(nil)
