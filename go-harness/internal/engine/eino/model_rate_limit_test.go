package eino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	openaisdk "github.com/meguminnnnnnnnn/go-openai"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func rateLimitFixtureError() error {
	return &openaiapi.APIError{HTTPStatusCode: http.StatusTooManyRequests, Message: "private free-model rate limit"}
}

func rateLimitTracked(ctx context.Context, inner model.ToolCallingChatModel, limits harness.BudgetLimits) *trackedModel {
	return &trackedModel{inner: &outputTokenModel{inner: inner, provider: "injected"}, nativeEndpoint: true, io: newRunIO(ctx), budget: &runBudget{limits: limits}, sink: &eventSink{}}
}

func callRateLimitTracked(ctx context.Context, m *trackedModel, mode string) (string, error) {
	input := []*schema.Message{schema.UserMessage("fixture")}
	if mode == "generate" {
		answer, err := m.Generate(ctx, input)
		if answer == nil {
			return "", err
		}
		return answer.Content, err
	}
	stream, err := m.Stream(ctx, input)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var text strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return text.String(), nil
		}
		if err != nil {
			return text.String(), err
		}
		if chunk != nil {
			text.WriteString(chunk.Content)
		}
	}
}

func TestModelRateLimitClassifiesOnlyTypedProviderFailures(t *testing.T) {
	for _, cause := range []error{
		rateLimitFixtureError(),
		&openaisdk.RequestError{HTTPStatusCode: http.StatusTooManyRequests, Err: errors.New("private body")},
		&anthropic.Error{StatusCode: http.StatusTooManyRequests},
	} {
		wrapped := fmt.Errorf("provider: %w", cause)
		got := (&trackedModel{}).modelError(wrapped)
		if !errors.Is(got, harness.ErrModelRateLimited) || !errors.Is(got, cause) || retryableModelRateLimit(got) {
			t.Fatalf("classified error lost cause or authorized replay: %v", got)
		}
		if !retryableModelRateLimit(rejectedRateLimitRequest(wrapped)) {
			t.Fatalf("rejected request did not retain retry classification: %v", wrapped)
		}
		for _, other := range []error{context.Canceled, &budgetError{resource: "tokens"}, &durablebudget.PersistenceError{Operation: "settle", Err: errors.New("storage failed")}} {
			if retryableModelRateLimit(rejectedRateLimitRequest(errors.Join(wrapped, other))) {
				t.Fatalf("joined failure authorized replay: %v", other)
			}
		}
	}
	for _, cause := range []error{nil, errors.New("status code 429"), context.Canceled, &openaiapi.APIError{HTTPStatusCode: http.StatusUnauthorized}} {
		if got := classifyModelRateLimitError(cause); got != cause {
			t.Fatalf("non-rate-limit cause was changed: %v", got)
		}
	}
}

func TestProviderRateLimitHTTPRecoversAndMetersEveryAttempt(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":{"message":"Too many requests for this free model","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
					return
				}
				_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			chat, err := newModel(ctx, Config{Provider: "openai-compatible", Model: "fixture", BaseURL: server.URL, APIKey: "fixture"}, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			m := &trackedModel{inner: chat, nativeEndpoint: true, io: newRunIO(ctx), budget: &runBudget{limits: harness.BudgetLimits{MaxModelCalls: 4, MaxOutputTokens: 64}}, sink: &eventSink{}}
			answer, err := callRateLimitTracked(ctx, m, mode)
			m.io.closeAndWait()
			snapshot, snapshotErr := m.budget.snapshot()
			if err != nil || snapshotErr != nil || answer != "recovered" || calls.Load() != 2 || snapshot.ModelCalls != 2 || snapshot.Tokens <= 11 {
				t.Fatalf("answer=%q err=%v calls=%d budget=%+v snapshotErr=%v", answer, err, calls.Load(), snapshot, snapshotErr)
			}
		})
	}
}

func TestModelRateLimitRetriesAreBoundedWithBackoff(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				wantAt := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}
				fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
					if call >= len(wantAt) || time.Since(start) != wantAt[call] {
						t.Fatalf("unexpected retry %d at %v", call, time.Since(start))
					}
					return nil, rateLimitFixtureError()
				}}
				m := rateLimitTracked(context.Background(), fake, harness.BudgetLimits{MaxModelCalls: 8, MaxOutputTokens: 64})
				_, err := callRateLimitTracked(context.Background(), m, mode)
				m.io.closeAndWait()
				snapshot, snapshotErr := m.budget.snapshot()
				if !errors.Is(err, harness.ErrModelRateLimited) || fake.calls != 4 || snapshotErr != nil || snapshot.ModelCalls != 4 || snapshot.Tokens <= 0 {
					t.Fatalf("err=%v calls=%d budget=%+v snapshotErr=%v", err, fake.calls, snapshot, snapshotErr)
				}
			})
		})
	}
}

func TestProviderRateLimitClaudeHTTPRequestsMatchBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		rejected int32
		want     int32
	}{
		{"generate-recovers", "generate", 1, 2},
		{"stream-recovers", "stream", 1, 2},
		{"exhausted", "generate", 4, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) <= tc.rejected {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"fixture rate limit"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"fixture\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			chat, err := newModel(ctx, Config{Provider: "claude", Model: "fixture", BaseURL: server.URL, APIKey: "fixture"}, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			m := &trackedModel{inner: chat, nativeEndpoint: true, io: newRunIO(ctx), budget: &runBudget{limits: harness.BudgetLimits{MaxModelCalls: 8, MaxOutputTokens: 64}}, sink: &eventSink{}}
			answer, err := callRateLimitTracked(ctx, m, tc.mode)
			m.io.closeAndWait()
			snapshot, snapshotErr := m.budget.snapshot()
			exhausted := tc.rejected == tc.want
			if snapshotErr != nil || calls.Load() != tc.want || snapshot.ModelCalls != int(tc.want) || exhausted && !errors.Is(err, harness.ErrModelRateLimited) || !exhausted && (err != nil || answer != "recovered") {
				t.Fatalf("answer=%q err=%v calls=%d budget=%+v snapshotErr=%v", answer, err, calls.Load(), snapshot, snapshotErr)
			}
		})
	}
}

func TestModelRateLimitCancellationStopsBackoff(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
					return nil, rateLimitFixtureError()
				}}
				m := rateLimitTracked(ctx, fake, harness.BudgetLimits{MaxModelCalls: 8})
				start := time.Now()
				_, err := callRateLimitTracked(ctx, m, mode)
				m.io.closeAndWait()
				if !errors.Is(err, context.DeadlineExceeded) || fake.calls != 1 || time.Since(start) != 500*time.Millisecond {
					t.Fatalf("err=%v calls=%d elapsed=%v", err, fake.calls, time.Since(start))
				}
			})
		})
	}
}

func TestModelRateLimitHonorsBudgetAndSettlementFailure(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		for _, blocker := range []string{"budget", "settlement", "auxiliary"} {
			t.Run(mode+"/"+blocker, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					failure := errors.New("usage persistence failed")
					fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
						return nil, rateLimitFixtureError()
					}}
					m := rateLimitTracked(context.Background(), fake, harness.BudgetLimits{MaxModelCalls: 1})
					if blocker == "settlement" {
						m.sink.callback = func(_ context.Context, e harness.RunEvent) error {
							if e.Kind == "usage" {
								return failure
							}
							return nil
						}
					} else if blocker == "auxiliary" {
						m.nativeEndpoint = false
					}
					_, err := callRateLimitTracked(context.Background(), m, mode)
					m.io.closeAndWait()
					var limit *budgetError
					if fake.calls != 1 || m.budget.modelCalls != 1 || m.budget.held != 0 || blocker == "budget" && (!errors.As(err, &limit) || limit.resource != "model_calls") || blocker == "settlement" && !errors.Is(err, failure) || blocker == "auxiliary" && !errors.Is(err, harness.ErrModelRateLimited) {
						t.Fatalf("blocker=%s err=%v calls=%d budget=%+v", blocker, err, fake.calls, m.budget)
					}
				})
			})
		}
	}
}

func TestModelRateLimitAfterPartialStreamDoesNotReplay(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
					reader, writer := schema.Pipe[*schema.Message](2)
					writer.Send(schema.AssistantMessage("partial output", nil), nil)
					writer.Send(nil, rateLimitFixtureError())
					writer.Close()
					return reader, nil
				}}
				m := rateLimitTracked(context.Background(), fake, harness.BudgetLimits{MaxModelCalls: 8})
				answer, err := callRateLimitTracked(context.Background(), m, mode)
				m.io.closeAndWait()
				if !errors.Is(err, harness.ErrModelRateLimited) || retryableModelRateLimit(err) || fake.calls != 1 || mode == "stream" && answer != "partial output" || mode == "generate" && answer != "" {
					t.Fatalf("answer=%q err=%v calls=%d", answer, err, fake.calls)
				}
			})
		})
	}
}

func TestModelRateLimitEngineKeepsCompletedToolAndDurableAccounting(t *testing.T) {
	t.Parallel()
	limits := harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 1, MaxTokens: 100000, MaxOutputTokens: 64}
	_, ledger, req, scope := durableFixture(t, "rate-limit-tool", limits)
	target := &recordingTool{}
	fake := &scriptedModel{stream: func(ctx context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return limitedOutput("", "tool_calls", schema.ToolCall{ID: "completed-call", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"preserved"}`}}), nil
		}
		found := false
		for _, msg := range input {
			if msg.Role == schema.Tool && msg.ToolCallID == "completed-call" && strings.Contains(msg.Content, "preserved") {
				found = true
			}
		}
		if !found || target.calls.Load() != 1 {
			t.Errorf("model call %d lost the completed tool result or replayed the tool: found=%t tools=%d", call, found, target.calls.Load())
		}
		if call == 1 {
			return nil, rateLimitFixtureError()
		}
		state, err := ledger.Snapshot(ctx, scope.RootBudgetID)
		if err != nil || state.ModelCalls != 3 || state.ToolCalls != 1 || state.SpentTokens <= 60 {
			t.Errorf("failed model attempt was not settled before retry: state=%+v err=%v", state, err)
		}
		return limitedOutput("completed", "stop"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{target}, Budget: limits, BudgetLedger: ledger})
	ctx, cancel := context.WithTimeout(durablebudget.WithScope(context.Background(), scope), 10*time.Second)
	defer cancel()
	result, err := e.Run(ctx, req, nil, allowTool)
	state, readErr := ledger.Snapshot(context.Background(), scope.RootBudgetID)
	if err != nil || readErr != nil || result.StopReason != "end_turn" || fake.calls != 3 || target.calls.Load() != 1 || state.ModelCalls != 3 || state.ToolCalls != 1 || state.HeldTokens != 0 || state.SpentTokens <= 120 {
		t.Fatalf("result=%+v err=%v calls=%d tools=%d state=%+v readErr=%v", result, err, fake.calls, target.calls.Load(), state, readErr)
	}
}

func TestModelRateLimitDurableSettlementFailureBlocksRetry(t *testing.T) {
	t.Parallel()
	limits := harness.BudgetLimits{MaxModelCalls: 4, MaxTokens: 100000, MaxOutputTokens: 64}
	store, ledger, req, scope := durableFixture(t, "rate-limit-settlement", limits)
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		_, err := store.DB().Exec(`CREATE TRIGGER fail_rate_limit_settlement BEFORE UPDATE OF spent_tokens ON budget_roots WHEN NEW.spent_tokens>OLD.spent_tokens BEGIN SELECT RAISE(ABORT,'injected rate-limit settlement failure'); END;`)
		if err != nil {
			return nil, err
		}
		return nil, rateLimitFixtureError()
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Budget: limits, BudgetLedger: ledger})
	_, err := e.Run(durablebudget.WithScope(context.Background(), scope), req, nil, nil)
	var persistence *durablebudget.PersistenceError
	state, readErr := ledger.Snapshot(context.Background(), scope.RootBudgetID)
	if !errors.As(err, &persistence) || fake.calls != 1 || readErr != nil || state.ModelCalls != 1 || state.HeldTokens <= 0 {
		t.Fatalf("err=%v calls=%d state=%+v readErr=%v", err, fake.calls, state, readErr)
	}
}
