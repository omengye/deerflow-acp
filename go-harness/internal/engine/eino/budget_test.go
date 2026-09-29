package eino

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func allowTool(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
	return harness.AllowOnce, nil
}

func TestBudgetStopsModelLoopBeforeNextRequest(t *testing.T) {
	underlying := &recordingTool{}
	fake := toolScript()
	e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Budget: harness.BudgetLimits{MaxModelCalls: 1}})
	var events []harness.RunEvent
	result, err := e.Run(context.Background(), request("model-budget"), func(_ context.Context, ev harness.RunEvent) error { events = append(events, ev); return nil }, allowTool)
	if err != nil || result.StopReason != "max_turn_requests" || result.Limit != "model_calls" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if fake.calls != 1 || underlying.calls.Load() != 1 {
		t.Fatalf("model=%d tools=%d", fake.calls, underlying.calls.Load())
	}
	if events[len(events)-1].Kind != "budget_exhausted" {
		t.Fatalf("missing terminal budget event: %+v", events)
	}
}

func TestToolBudgetStopsBeforePermissionOrSideEffect(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: fmt.Sprintf("call-%d", call), Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
	}}
	underlying := &recordingTool{}
	e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Budget: harness.BudgetLimits{MaxToolCalls: 1}})
	var approvals atomic.Int32
	result, err := e.Run(context.Background(), request("tool-budget"), nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals.Add(1)
		return harness.AllowOnce, nil
	})
	if err != nil || result.Limit != "tool_calls" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if approvals.Load() != 1 || underlying.calls.Load() != 1 {
		t.Fatalf("approvals=%d tools=%d", approvals.Load(), underlying.calls.Load())
	}
}

func TestParentAndSubagentShareModelBudget(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"general-purpose","prompt":"inspect","description":"inspect"}`}}}}}), nil
		}
		return textStream("child must not execute"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Budget: harness.BudgetLimits{MaxModelCalls: 1}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(context.Background(), request("child-budget"), nil, allowTool)
	if err != nil || result.Limit != "model_calls" || fake.calls != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, fake.calls, err)
	}
}

func TestTokenReservationsCapOutputAndSettleActualUsage(t *testing.T) {
	b := &runBudget{limits: harness.BudgetLimits{MaxTokens: 1000, MaxOutputTokens: 50}}
	input := []*schema.Message{{Role: schema.User, Content: "hello"}}
	r, opts, err := b.reserve(input, []model.Option{model.WithMaxTokens(200)})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.GetCommonOptions(&model.Options{}, opts...).MaxTokens; got == nil || *got != 50 {
		t.Fatalf("output cap=%v", got)
	}
	for _, used := range []int{10, 20, 20} {
		if err := r.observe(&schema.Message{Content: "text", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: used, CompletionTokens: 5, TotalTokens: used + 5}}}); err != nil {
			t.Fatal(err)
		}
	}
	u := r.settle()
	r.settle()
	if b.spent != 25 || b.held != 0 || u.Estimated || u.InputTokens != 20 {
		t.Fatalf("usage=%+v spent=%d held=%d", u, b.spent, b.held)
	}
	r, _, err = b.reserve(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.observe(&schema.Message{Content: "12345678"}); err != nil {
		t.Fatal(err)
	}
	u = r.settle()
	if !u.Estimated || u.OutputTokens != 2 || u.InputTokens == 0 {
		t.Fatalf("estimated usage=%+v", u)
	}
}

func TestConcurrentReservationsCannotOverbook(t *testing.T) {
	b := &runBudget{limits: harness.BudgetLimits{MaxTokens: 1000, MaxOutputTokens: 50}}
	var wg sync.WaitGroup
	var started atomic.Int32
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := b.reserve([]*schema.Message{{Role: schema.User, Content: "x"}}, nil); err == nil {
				started.Add(1)
			}
		}()
	}
	wg.Wait()
	if b.held > 1000 || started.Load() == 0 || started.Load() == 100 || b.failure() == nil {
		t.Fatalf("held=%d started=%d failure=%v", b.held, started.Load(), b.failure())
	}
}

func TestTimeoutWaitsForProviderCleanup(t *testing.T) {
	var cleaned atomic.Bool
	fake := &scriptedModel{stream: func(ctx context.Context, _ int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		r, w := schema.Pipe[*schema.Message](1)
		go func() {
			defer w.Close()
			<-ctx.Done()
			w.Send(nil, ctx.Err())
			time.Sleep(20 * time.Millisecond)
			cleaned.Store(true)
		}()
		return r, nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Budget: harness.BudgetLimits{Timeout: 100 * time.Millisecond}})
	result, err := e.Run(context.Background(), request("time-budget"), nil, nil)
	if err != nil || result.StopReason != "cancelled" || result.Limit != "time" || !cleaned.Load() {
		t.Fatalf("result=%+v err=%v cleaned=%v", result, err, cleaned.Load())
	}
}

func TestEarlyTimeoutKeepsCleanupFailure(t *testing.T) {
	cleanupFailure := errors.New("cleanup failed independently")
	e := newTestEngine(t, Config{ChatModel: toolScript(), Budget: harness.BudgetLimits{Timeout: time.Millisecond}, ToolFactory: func(ctx context.Context, _ harness.RunRequest) ([]tool.BaseTool, func() error, error) {
		<-ctx.Done()
		return nil, func() error { return cleanupFailure }, ctx.Err()
	}})
	result, err := e.Run(context.Background(), request("setup-timeout"), nil, nil)
	if result.Limit != "time" || !errors.Is(err, cleanupFailure) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	joined := fmt.Errorf("wrapped: %w", errors.Join(&budgetError{"tokens"}, cleanupFailure, context.Canceled))
	if got := withoutBudgetTermination(joined); !errors.Is(got, cleanupFailure) {
		t.Fatalf("lost failure: %v", got)
	}
}

func TestActualTokenUsageCanExceedEstimateAndStopsRun(t *testing.T) {
	b := &runBudget{limits: harness.BudgetLimits{MaxTokens: 100, MaxOutputTokens: 10}}
	r, _, err := b.reserve([]*schema.Message{{Role: schema.User, Content: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.observe(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 110, CompletionTokens: 10, TotalTokens: 120}}}); err == nil {
		t.Fatal("actual provider overrun ignored")
	}
	r.settle()
	if _, _, err = b.reserve(nil, nil); err == nil {
		t.Fatal("new request after token exhaustion")
	}
}

func TestCancellationDoesNotHideIndependentProviderFailure(t *testing.T) {
	providerFailure := errors.New("provider cleanup independently failed")
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprint(streamed), func(t *testing.T) {
			fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				joined := errors.Join(context.Canceled, providerFailure)
				if !streamed {
					return nil, joined
				}
				r, w := schema.Pipe[*schema.Message](1)
				w.Send(nil, joined)
				w.Close()
				return r, nil
			}}
			e := newTestEngine(t, Config{ChatModel: fake})
			_, err := e.Run(context.Background(), request("mixed-error"), nil, nil)
			if !errors.Is(err, providerFailure) {
				t.Fatalf("independent provider error lost: %v", err)
			}
		})
	}
}

func TestProviderDrainFailureSurvivesEarlierCancellation(t *testing.T) {
	cleanupFailure := errors.New("failure after cancellation while draining")
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		r, w := schema.Pipe[*schema.Message](2)
		w.Send(nil, context.Canceled)
		w.Send(nil, cleanupFailure)
		w.Close()
		return r, nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	_, err := e.Run(context.Background(), request("drain-failure"), nil, nil)
	if !errors.Is(err, cleanupFailure) {
		t.Fatalf("drain error lost: %v", err)
	}
}
