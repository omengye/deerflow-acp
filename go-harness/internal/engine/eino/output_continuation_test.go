package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/backgroundtask"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func limitedOutput(text, reason string, calls ...schema.ToolCall) *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: text, ToolCalls: calls, ResponseMeta: &schema.ResponseMeta{FinishReason: reason, Usage: &schema.TokenUsage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60}}}})
}

func TestOutputContinuationStreamsPersistsAndMeters(t *testing.T) {
	for _, reason := range []string{"length", "max_tokens"} {
		t.Run(reason, func(t *testing.T) {
			store := einosession.NewInMemoryStore[*schema.Message](nil)
			fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return limitedOutput("first half ", reason), nil
				}
				found := false
				for _, msg := range input {
					found = found || msg.Role == schema.Assistant && strings.HasPrefix(msg.Content, "first half ")
				}
				if !found {
					t.Error("continuation or next turn lost the partial answer")
				}
				return limitedOutput("second half.", "stop"), nil
			}}
			var postRunAnswer string
			e := newTestEngine(t, Config{ChatModel: fake, SessionStore: store, Budget: harness.BudgetLimits{MaxModelCalls: 2, MaxOutputTokens: 100}, ExtensionFactory: func(context.Context, harness.RunRequest, json.RawMessage) (RunExtensions, error) {
				return RunExtensions{PostRunFactory: func(context.Context, model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
					return func(_ context.Context, answer string, _ bool) error { postRunAnswer = answer; return nil }, nil
				}}, nil
			}})
			// Inspect normal output and cumulative usage before the next turn.
			var text strings.Builder
			var usage harness.Usage
			result, err := e.Run(context.Background(), request("continue"), func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == "text_delta" {
					text.WriteString(event.Text)
				}
				if event.Usage != nil {
					usage.InputTokens += event.Usage.InputTokens
					usage.OutputTokens += event.Usage.OutputTokens
					usage.TotalTokens += event.Usage.TotalTokens
				}
				return nil
			}, nil)
			if err != nil || result.StopReason != "end_turn" || fake.calls != 2 || text.String() != "first half second half." || postRunAnswer != text.String() || usage.OutputTokens != 20 {
				t.Fatalf("result=%+v err=%v calls=%d text=%q usage=%+v", result, err, fake.calls, postRunAnswer, usage)
			}
			if _, err := e.Run(context.Background(), request("next-turn"), nil, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputContinuationRejectsTruncatedToolsWithoutReplay(t *testing.T) {
	for _, args := range []string{`{"path":`, `{}`} {
		t.Run(args, func(t *testing.T) {
			target := &recordingTool{}
			fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				switch call {
				case 0:
					return limitedOutput("", "tool_calls", schema.ToolCall{ID: "completed", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}), nil
				case 1:
					return limitedOutput("", "length", schema.ToolCall{ID: "truncated", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: args}}), nil
				case 2:
					for _, msg := range input {
						for _, call := range msg.ToolCalls {
							if call.ID == "truncated" {
								t.Error("truncated arguments leaked into next request")
							}
						}
					}
					return limitedOutput("", "tool_calls", schema.ToolCall{ID: "reissued", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}), nil
				default:
					return limitedOutput("done", "stop"), nil
				}
			}}
			e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{target}})
			var starts int
			result, err := e.Run(context.Background(), request("tools"), func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == "tool_start" {
					starts++
					if event.ToolCallID == "truncated" {
						t.Error("truncated tool was executed")
					}
				}
				return nil
			}, allowTool)
			if err != nil || result.StopReason != "end_turn" || fake.calls != 4 || starts != 2 {
				t.Fatalf("result=%+v err=%v calls=%d starts=%d", result, err, fake.calls, starts)
			}
		})
	}
}

func TestOutputContinuationBoundedAndExplicit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		progress bool
		want     int
	}{
		{"no-visible-progress", false, 2},
		{"bounded-progress", true, maxOutputContinuations + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				text := ""
				if tc.progress {
					text = fmt.Sprintf("part-%d ", call)
				}
				return limitedOutput(text, "length"), nil
			}}
			e := newTestEngine(t, Config{ChatModel: fake})
			var notice int
			result, err := e.Run(context.Background(), request(tc.name), func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == "output_limit" {
					notice++
				}
				return nil
			}, nil)
			if err != nil || result.StopReason != "max_tokens" || result.Limit != "" || fake.calls != tc.want || notice != 1 {
				t.Fatalf("result=%+v err=%v calls=%d notices=%d", result, err, fake.calls, notice)
			}
		})
	}
}

func TestOutputContinuationHonorsSharedBudget(t *testing.T) {
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return limitedOutput("partial", "length"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Budget: harness.BudgetLimits{MaxModelCalls: 1, MaxOutputTokens: 100}})
	result, err := e.Run(context.Background(), request("budget"), nil, nil)
	if err != nil || result.Limit != "model_calls" || fake.calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, fake.calls)
	}
}

func TestOutputContinuationStopsAfterRepeatedAnswer(t *testing.T) {
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return limitedOutput("identical partial answer", "length"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	result, err := e.Run(context.Background(), request("repeat"), nil, nil)
	if err != nil || result.StopReason != "max_tokens" || fake.calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, fake.calls)
	}
}

func TestOutputContinuationSettlementFailureDoesNotRetry(t *testing.T) {
	failure := errors.New("usage storage failed")
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return limitedOutput("partial", "length"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	_, err := e.Run(context.Background(), request("failed-settlement"), func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "usage" {
			return failure
		}
		return nil
	}, nil)
	if !errors.Is(err, failure) || fake.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, fake.calls)
	}
}

func TestOutputContinuationPreservesFinalIncompleteTextForFollowup(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call <= maxOutputContinuations {
			return limitedOutput(fmt.Sprintf("part-%d", call), "length"), nil
		}
		found := false
		for _, msg := range input {
			found = found || strings.Contains(msg.Content, fmt.Sprintf("part-%d", maxOutputContinuations))
		}
		if !found {
			t.Error("follow-up lost the last truncated answer")
		}
		return limitedOutput("completed", "stop"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	result, err := e.Run(context.Background(), request("incomplete"), nil, nil)
	if err != nil || result.StopReason != "max_tokens" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = e.Run(context.Background(), request("follow-up"), nil, nil)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestOutputContinuationDurableAccounting(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 2, MaxTokens: 100000, MaxOutputTokens: 100}
	_, ledger, req, scope := durableFixture(t, "continuation-ledger", limits)
	fake := &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return limitedOutput("first ", "length"), nil
		}
		snapshot, err := ledger.Snapshot(ctx, scope.RootBudgetID)
		if err != nil || snapshot.SpentTokens != 60 || snapshot.ModelCalls != 2 {
			t.Errorf("previous request not settled before retry: snapshot=%+v err=%v", snapshot, err)
		}
		return limitedOutput("last", "stop"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Budget: limits, BudgetLedger: ledger})
	result, err := e.Run(durablebudget.WithScope(context.Background(), scope), req, nil, nil)
	snapshot, readErr := ledger.Snapshot(context.Background(), scope.RootBudgetID)
	if err != nil || readErr != nil || result.StopReason != "end_turn" || snapshot.SpentTokens != 120 || snapshot.ModelCalls != 2 || snapshot.HeldTokens != 0 {
		t.Fatalf("result=%+v err=%v snapshot=%+v readErr=%v", result, err, snapshot, readErr)
	}
}

func TestOutputContinuationPreparedObserver(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return limitedOutput("first ", "length"), nil
		}
		return limitedOutput("last", "stop"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	var text strings.Builder
	p, err := e.PrepareAttempt(context.Background(), request("prepared-output"), "worker-v1", nil, func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "text_delta" {
			text.WriteString(event.Text)
		}
		return nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := observerExecutor(t, e, p, ExecutionHooks{}, "worker-v1", nil)
	out, err := executor.Execute(context.Background(), &backgroundtask.Task{Spec: observerSpec(t, request("prepared-output"), "worker-v1"), Attempt: 1}, observerRuntime{})
	joinErr := p.JoinAndClose(context.Background())
	if err != nil || joinErr != nil || fake.calls != 2 || text.String() != "first last" || out == nil || string(out.Data) != "first last" {
		t.Fatalf("err=%v joinErr=%v calls=%d text=%q output=%+v", err, joinErr, fake.calls, text.String(), out)
	}
}

func TestOutputContinuationNonStreaming(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return limitedOutput("first ", "length"), nil
		}
		return limitedOutput("last", "stop"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	p, err := e.PrepareAttempt(context.Background(), request("non-stream"), "worker-v1", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
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

func TestOutputContinuationConfiguredRole(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return limitedOutput("", "tool_calls", schema.ToolCall{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}}), nil
		case 1:
			return limitedOutput("child prefix ", "length"), nil
		case 2:
			return limitedOutput("child suffix", "stop"), nil
		default:
			found := false
			for _, msg := range input {
				if msg.Role == schema.Tool && strings.Contains(msg.Content, "child prefix ") && strings.Contains(msg.Content, "child suffix") {
					found = true
				}
			}
			if !found {
				t.Error("parent did not receive the complete continued role answer")
			}
			return limitedOutput("parent complete", "stop"), nil
		}
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(context.Background(), request("role-continue"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 4 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, fake.calls)
	}
}

func TestOutputContinuationFailedRoleReturnsControlToParent(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return limitedOutput("", "tool_calls", schema.ToolCall{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}}), nil
		}
		if call < 3 {
			return limitedOutput("", "length"), nil
		}
		return limitedOutput("The child could not complete; here are the confirmed results.", "stop"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(context.Background(), request("role-output-failed"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 4 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, fake.calls)
	}
}

func TestOutputContinuationDoesNotGuessOrRetryErrors(t *testing.T) {
	for _, reason := range []string{"stop", "content_filter", "refusal", "network"} {
		t.Run(reason, func(t *testing.T) {
			failure := errors.New("provider disconnected")
			fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if reason == "network" {
					return nil, failure
				}
				return limitedOutput("unfinished sentence at", reason), nil
			}}
			e := newTestEngine(t, Config{ChatModel: fake})
			_, err := e.Run(context.Background(), request(reason), nil, nil)
			if fake.calls != 1 || (reason == "network") != errors.Is(err, failure) {
				t.Fatalf("calls=%d err=%v", fake.calls, err)
			}
		})
	}
}

func TestOutputContinuationCompactsBeforeNextRequest(t *testing.T) {
	summaries, steps := 0, 0
	fake := &scriptedModel{stream: func(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if strings.Contains(input[0].Content, "tasked with summarizing conversations") || strings.Contains(input[0].Content, "负责总结对话") {
			summaries++
			return textStream("Earlier context summarized."), nil
		}
		steps++
		if steps == 1 {
			return limitedOutput("partial", "length"), nil
		}
		if summaries != 1 {
			t.Error("continuation did not recheck compaction")
		}
		return limitedOutput(" done", "stop"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 4, ContextTokens: 100000, KeepRecentMessages: 1}})
	req := request("compact")
	req.History = []harness.Message{{Role: "user", Content: []harness.Content{{Type: "text", Text: "old task"}}}, {Role: "assistant", Content: []harness.Content{{Type: "text", Text: "old answer"}}}}
	result, err := e.Run(context.Background(), req, nil, nil)
	if err != nil || result.StopReason != "end_turn" || steps != 2 || summaries != 1 {
		t.Fatalf("result=%+v err=%v steps=%d summaries=%d", result, err, steps, summaries)
	}
}

func TestOutputContinuationCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			cancel()
		}
		return limitedOutput("partial", "length"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	result, err := e.Run(ctx, request("cancel"), nil, nil)
	if err != nil || result.StopReason != "cancelled" || fake.calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, fake.calls)
	}
}

func TestCompactionRejectsTruncatedSummary(t *testing.T) {
	original := []*schema.Message{schema.UserMessage("old"), schema.AssistantMessage("old", nil), schema.UserMessage("current")}
	_, err := finalizeCompaction(original, &schema.Message{Role: schema.Assistant, Content: "incomplete", ResponseMeta: &schema.ResponseMeta{FinishReason: "length"}}, 1)
	if err == nil {
		t.Fatal("truncated summary replaced intact history")
	}
}
