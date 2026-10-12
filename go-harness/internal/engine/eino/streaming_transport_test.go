package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestModelConstructorsAndToolBindingAlwaysUseStreaming(t *testing.T) {
	for _, provider := range []string{"", "openai", "openai-compatible", "claude", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body["tools"].([]any)) != 1 {
					t.Error("tool binding was lost")
				}
				if provider == "claude" || provider == "anthropic" {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"fixture\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
				}
			}))
			defer server.Close()
			for _, maintenance := range []bool{false, true} {
				chat, err := newModelWithRetryPolicy(context.Background(), Config{Provider: provider, Model: "fixture", APIKey: "fixture", BaseURL: server.URL}, "fixture", maintenance)
				if err != nil {
					t.Fatal(err)
				}
				chat, err = chat.WithTools([]*schema.ToolInfo{{Name: "noop", Desc: "Do nothing"}})
				if err != nil {
					t.Fatal(err)
				}
				answer, err := chat.Generate(context.Background(), []*schema.Message{schema.UserMessage("Reply OK")}, model.WithMaxTokens(384))
				if err != nil || answer.Content != "OK" || answer.ResponseMeta.Usage.TotalTokens != 11 {
					t.Fatalf("answer=%+v err=%v", answer, err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("unexpected retry or duplicate request: %d", calls.Load())
			}
		})
	}
}

func TestModelConnectivityProbeUsesStreaming(t *testing.T) {
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	if err := ProbeModel(context.Background(), "openai", "fixture", server.URL, "fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCompactionUsesStreamingHTTP(t *testing.T) {
	var summaries atomic.Int32
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		answer := "done"
		if len(request.Messages) > 0 && (strings.Contains(request.Messages[0].Content, "tasked with summarizing conversations") || strings.Contains(request.Messages[0].Content, "负责总结对话")) {
			summaries.Add(1)
			answer = "The earlier task has been answered."
		}
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
	}))
	defer server.Close()
	e := newTestEngine(t, Config{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture", Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 5, KeepRecentMessages: 2}})
	for i := 0; i < 6; i++ {
		result, err := e.Run(context.Background(), request(fmt.Sprintf("summary-%d", i)), nil, nil)
		if err != nil || result.StopReason != "end_turn" {
			t.Fatalf("run=%+v err=%v", result, err)
		}
	}
	if summaries.Load() == 0 {
		t.Fatal("summary path did not run")
	}
}

type noGenerateModel struct{ *scriptedModel }

func (*noGenerateModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	panic("injected provider Generate must not be called")
}

func (m *noGenerateModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	_, err := m.scriptedModel.WithTools(tools)
	return m, err
}

func TestNativeSynchronousRunnerKeepsStreamingForInjectedModel(t *testing.T) {
	fake := &noGenerateModel{scriptedModel: &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return limitedOutput("first ", "length"), nil
		}
		return limitedOutput("last", "stop"), nil
	}}}
	e := newTestEngine(t, Config{ChatModel: fake})
	p, err := e.PrepareAttempt(context.Background(), request("sync-stream"), "worker-v1", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.JoinAndClose(context.Background())
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: p.Agent})
	iter := runner.Run(context.Background(), []*schema.Message{schema.UserMessage("finish")})
	var answer string
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Error(event.Err)
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			msg, err := event.Output.MessageOutput.GetMessage()
			if err != nil {
				t.Error(err)
			} else {
				answer = msg.Content
			}
		}
	}
	if err := p.JoinAndClose(context.Background()); err != nil || answer != "first last" || fake.calls != 2 {
		t.Fatalf("err=%v answer=%q calls=%d", err, answer, fake.calls)
	}
}

func TestTrackedGenerateKeepsUsageOnStreamFailure(t *testing.T) {
	failure := errors.New("fixture stream failure")
	finished := make(chan struct{})
	fake := &noGenerateModel{scriptedModel: &scriptedModel{stream: func(work context.Context, _ int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		reader, writer := schema.Pipe[*schema.Message](1)
		go func() {
			defer writer.Close()
			defer close(finished)
			writer.Send(schema.AssistantMessage("partial", nil), nil)
			writer.Send(nil, failure)
			<-work.Done()
			writer.Send(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}}}, nil)
		}()
		return reader, nil
	}}}
	b := &runBudget{limits: harness.BudgetLimits{MaxModelCalls: 1, MaxTokens: 10000, MaxOutputTokens: 64}}
	work := newRunIO(context.Background())
	tracked := &trackedModel{inner: fake, io: work, budget: b, sink: &eventSink{}}
	answer, err := tracked.Generate(context.Background(), []*schema.Message{schema.UserMessage("fixture")})
	work.closeAndWait()
	if answer != nil || !errors.Is(err, failure) || b.held != 0 || b.spent != 13 || fake.calls != 1 {
		t.Fatalf("answer=%+v err=%v held=%d spent=%d calls=%d", answer, err, b.held, b.spent, fake.calls)
	}
	if work.failure() != nil {
		t.Fatal("optional model error unexpectedly failed the owning run")
	}
	select {
	case <-finished:
	default:
		t.Fatal("provider was not joined")
	}
}
