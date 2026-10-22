package eino

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func TestCompactionSummaryValidationReasons(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		summary      *schema.Message
	}{
		{"nil", harness.CompactionInvalid, nil},
		{"length", harness.CompactionTruncated, &schema.Message{Role: schema.Assistant, Content: "partial", ResponseMeta: &schema.ResponseMeta{FinishReason: "length"}}},
		{"reasoning-only", harness.CompactionTruncated, &schema.Message{Role: schema.Assistant, ReasoningContent: "private", ResponseMeta: &schema.ResponseMeta{FinishReason: "max_tokens"}}},
		{"empty", harness.CompactionEmpty, schema.AssistantMessage(" \n", nil)},
		{"oversized", harness.CompactionOversized, schema.AssistantMessage(strings.Repeat("x", maxSummaryBytes+1), nil)},
		{"role", harness.CompactionInvalid, schema.UserMessage("not a summary")},
		{"tool", harness.CompactionInvalid, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "never-execute"}}}},
		{"truncated-tool", harness.CompactionInvalid, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "never-execute"}}, ResponseMeta: &schema.ResponseMeta{FinishReason: "length"}}},
		{"utf8", harness.CompactionInvalid, schema.AssistantMessage(string([]byte{0xff}), nil)},
		{"valid", "", schema.AssistantMessage("The tool completed; continue with verification.", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCompactionSummary(tc.summary, 2)
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Reason != tc.reason || err.Attempts != 2 {
				t.Fatalf("reason=%+v expected=%s", err, tc.reason)
			}
			if tc.summary != nil && err.SummaryBytes != len(tc.summary.Content) {
				t.Fatal("summary byte count was lost")
			}
		})
	}
	unsafe := schema.AssistantMessage("", nil)
	unsafe.ResponseMeta = &schema.ResponseMeta{FinishReason: "private provider detail"}
	if got := validateCompactionSummary(unsafe, 1); got.FinishReason != "" || strings.Contains(got.Error(), "private") {
		t.Fatal("unrecognized provider data leaked into diagnostics")
	}
}

func TestCompactionRegeneratesRejectedSummaryFromOriginal(t *testing.T) {
	for _, reason := range []string{"length", "empty", "oversized"} {
		t.Run(reason, func(t *testing.T) {
			input := []*schema.Message{schema.SystemMessage("summary policy"), schema.UserMessage("original task and tool results")}
			fake := &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					switch reason {
					case "empty":
						return textStream(""), nil
					case "oversized":
						return textStream(strings.Repeat("x", maxSummaryBytes+1)), nil
					default:
						return limitedOutput("discarded partial summary", "length"), nil
					}
				}
				if len(messages) != len(input)+1 || messages[1] != input[1] || messages[len(messages)-1].Content != compactionRetryInstruction {
					t.Fatal("retry did not reuse original input with a shorter-summary instruction")
				}
				for _, m := range messages {
					if strings.Contains(m.Content, "discarded partial summary") {
						t.Fatal("rejected summary was reused")
					}
				}
				return textStream("Completed tool results preserved; verify next."), nil
			}}
			result, err := (&compactionModel{inner: fake}).Generate(context.Background(), input)
			if err != nil || fake.calls != 2 || result.Content == "" || len(input) != 2 {
				t.Fatalf("result=%+v calls=%d err=%v", result, fake.calls, err)
			}
		})
	}
}

func TestCompactionDoesNotRetryProviderFailureInvalidShapeOrCancellation(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	for _, mode := range []string{"provider", "invalid", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				switch mode {
				case "provider":
					return nil, providerErr
				case "invalid":
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "never-execute"}}}}), nil
				default:
					cancel()
					return limitedOutput("partial", "length"), nil
				}
			}}
			_, err := (&compactionModel{inner: fake}).Generate(ctx, []*schema.Message{schema.UserMessage("summary")})
			if err == nil || fake.calls != 1 {
				t.Fatalf("calls=%d err=%v", fake.calls, err)
			}
			if mode == "provider" && !errors.Is(err, providerErr) || mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cause lost: %v", err)
			}
		})
	}
}

func TestCompactionRetryFailurePreservesNativeHistory(t *testing.T) {
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call >= 2 {
			return limitedOutput("never persist this partial summary", "length"), nil
		}
		return textStream("prior answer"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, SessionStore: store, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 4, ContextTokens: 100000, KeepRecentMessages: 1}})
	for _, id := range []string{"first", "second"} {
		if _, err := e.Run(context.Background(), request(id), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.Run(context.Background(), request("third"), nil, nil)
	var failure *harness.CompactionError
	if !errors.As(err, &failure) || failure.Reason != harness.CompactionTruncated || failure.Attempts != 2 || failure.FinishReason != "length" || fake.calls != 4 {
		t.Fatalf("calls=%d failure=%+v err=%v", fake.calls, failure, err)
	}
	events, err := store.LoadEvents(context.Background(), "test-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Events {
		if event.Kind == adk.SessionEventMessagesReplaced {
			t.Fatal("failed retries replaced native history")
		}
	}
}

func TestCompactionRecoveryDoesNotReplayCompletedTools(t *testing.T) {
	steps, summaries := 0, 0
	underlying := &recordingTool{}
	fake := &scriptedModel{stream: func(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if strings.Contains(input[0].Content, "tasked with summarizing conversations") || strings.Contains(input[0].Content, "负责总结对话") {
			summaries++
			if summaries == 1 {
				return limitedOutput("discarded incomplete handoff", "length"), nil
			}
			return textStream("Completed reads are recorded. Continue remaining reads without repeating them."), nil
		}
		if steps == 6 {
			return textStream("All reads completed."), nil
		}
		steps++
		return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: fmt.Sprintf("read-%d", steps), Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}})}), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 6, KeepRecentMessages: 2}})
	result, err := e.Run(context.Background(), request("compaction-tool-recovery"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || steps != 6 || underlying.calls.Load() != 6 || summaries < 2 {
		t.Fatalf("result=%+v steps=%d tools=%d summaries=%d err=%v", result, steps, underlying.calls.Load(), summaries, err)
	}
}

func TestCompactionRetrySharesDurableBudget(t *testing.T) {
	for _, maxCalls := range []int{1, 3} {
		t.Run(string(rune('0'+maxCalls)), func(t *testing.T) {
			limits := harness.BudgetLimits{MaxModelCalls: maxCalls, MaxTokens: 100000, MaxOutputTokens: 4096}
			_, ledger, req, scope := durableFixture(t, "compact-budget", limits)
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return limitedOutput("partial", "length"), nil
				}
				return textStream("Complete short summary."), nil
			}}
			e := newTestEngine(t, Config{ChatModel: fake, Budget: limits, BudgetLedger: ledger, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 4, ContextTokens: 100000, KeepRecentMessages: 1}})
			req.History = []harness.Message{
				{Role: "user", Content: []harness.Content{{Type: "text", Text: "old request"}}},
				{Role: "assistant", Content: []harness.Content{{Type: "text", Text: "old answer"}}},
				{Role: "user", Content: []harness.Content{{Type: "text", Text: "second request"}}},
				{Role: "assistant", Content: []harness.Content{{Type: "text", Text: "second answer"}}},
			}
			result, err := e.Run(durablebudget.WithScope(context.Background(), scope), req, nil, nil)
			if maxCalls == 3 && err != nil {
				t.Fatal(err)
			}
			if maxCalls == 1 && (err != nil || result.StopReason != "max_turn_requests" || result.Limit != "model_calls") {
				t.Fatalf("retry bypassed the shared model-call limit: result=%+v err=%v", result, err)
			}
			snapshot, snapshotErr := ledger.Snapshot(context.Background(), scope.RootBudgetID)
			if snapshotErr != nil || int(snapshot.ModelCalls) != maxCalls || fake.calls != maxCalls || snapshot.HeldTokens != 0 {
				t.Fatalf("calls=%d snapshot=%+v err=%v snapshotErr=%v", fake.calls, snapshot, err, snapshotErr)
			}
		})
	}
}
