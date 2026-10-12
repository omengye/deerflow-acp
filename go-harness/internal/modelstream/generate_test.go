package modelstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type streamOnlyModel struct {
	stream func(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

func (*streamOnlyModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	panic("non-streaming provider API must not be called")
}

func (m *streamOnlyModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.stream(ctx, input, opts...)
}

func TestGenerateCollectsToolFragmentsReasoningAndUsageTail(t *testing.T) {
	index := 0
	calls := 0
	chat := &streamOnlyModel{stream: func(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
		calls++
		if len(input) != 1 || input[0].Content != "fixture" || *model.GetCommonOptions(nil, opts...).MaxTokens != 384 {
			t.Fatal("request or output cap changed")
		}
		return schema.StreamReaderFromArray([]*schema.Message{
			nil,
			{Role: schema.Assistant, Content: "first", ReasoningContent: "reason", ToolCalls: []schema.ToolCall{{Index: &index, ID: "call", Type: "function", Function: schema.FunctionCall{Name: "probe", Arguments: `{"marker":`}}}},
			{Content: " second", ReasoningContent: "ing", ToolCalls: []schema.ToolCall{{Index: &index, Function: schema.FunctionCall{Arguments: `"ok"}`}}}, ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_calls"}},
			{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 7, TotalTokens: 17}}},
		}), nil
	}}
	answer, err := Generate(context.Background(), chat, []*schema.Message{schema.UserMessage("fixture")}, model.WithMaxTokens(384))
	if err != nil || calls != 1 || answer.Content != "first second" || answer.ReasoningContent != "reasoning" {
		t.Fatalf("answer=%+v calls=%d err=%v", answer, calls, err)
	}
	if len(answer.ToolCalls) != 1 || answer.ToolCalls[0].Function.Name != "probe" || answer.ToolCalls[0].Function.Arguments != `{"marker":"ok"}` || answer.ToolCalls[0].ID != "call" {
		t.Fatalf("tool fragments=%+v", answer.ToolCalls)
	}
	if answer.ResponseMeta.FinishReason != "tool_calls" || answer.ResponseMeta.Usage.TotalTokens != 17 {
		t.Fatalf("lost terminal metadata: %+v", answer.ResponseMeta)
	}
}

func TestGenerateRejectsIncompleteAndUnboundedStreams(t *testing.T) {
	for _, mode := range []string{"nil", "empty", "provider-error", "partial-error", "bytes", "chunks", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			failure := errors.New("fixture stream failed")
			finished := make(chan struct{})
			calls := 0
			chat := &streamOnlyModel{stream: func(work context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
				calls++
				switch mode {
				case "nil":
					return nil, nil
				case "empty":
					return schema.StreamReaderFromArray([]*schema.Message{}), nil
				case "provider-error":
					return nil, failure
				}
				reader, writer := schema.Pipe[*schema.Message](1)
				go func() {
					defer writer.Close()
					defer close(finished)
					switch mode {
					case "partial-error":
						writer.Send(schema.AssistantMessage("partial", nil), nil)
						writer.Send(nil, failure)
					case "bytes":
						writer.Send(schema.AssistantMessage(strings.Repeat("x", maxResponseBytes), nil), nil)
					case "chunks":
						for i := 0; i <= maxResponseChunks; i++ {
							writer.Send(nil, nil)
						}
					case "cancelled":
						cancel()
						writer.Send(nil, work.Err())
					}
					<-work.Done()
					// Teardown follows the error. The collector must drain this too.
					writer.Send(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 3}}}, nil)
				}()
				return reader, nil
			}}
			answer, err := Generate(ctx, chat, nil)
			if err == nil || answer != nil || calls != 1 {
				t.Fatalf("partial response accepted or retried: answer=%+v calls=%d err=%v", answer, calls, err)
			}
			if (mode == "partial-error" || mode == "provider-error") && !errors.Is(err, failure) {
				t.Fatalf("lost provider cause: %v", err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if mode == "partial-error" || mode == "bytes" || mode == "chunks" || mode == "cancelled" {
				select {
				case <-finished:
				default:
					t.Fatal("returned before provider teardown")
				}
			}
		})
	}
}

func TestGenerateDoesNotDispatchCancelledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Generate(ctx, &streamOnlyModel{stream: func(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
		t.Fatal("cancelled request reached provider")
		return nil, nil
	}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGenerateCombinesSplitUsageCounters(t *testing.T) {
	chat := &streamOnlyModel{stream: func(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray([]*schema.Message{
			{Role: schema.Assistant, Content: "OK", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, TotalTokens: 10}}},
			{ResponseMeta: &schema.ResponseMeta{FinishReason: "end_turn", Usage: &schema.TokenUsage{CompletionTokens: 3, TotalTokens: 3}}},
		}), nil
	}}
	answer, err := Generate(context.Background(), chat, nil)
	if err != nil || answer.ResponseMeta.Usage.TotalTokens != 13 {
		t.Fatalf("answer=%+v err=%v", answer, err)
	}
}

func TestGeneratePreservesInconsistentUsageForConservativeAccounting(t *testing.T) {
	for _, total := range []int{5, 30} {
		chat := &streamOnlyModel{stream: func(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
			return schema.StreamReaderFromArray([]*schema.Message{
				{Role: schema.Assistant, Content: "OK", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: total}}},
			}), nil
		}}
		answer, err := Generate(context.Background(), chat, nil)
		if err != nil || answer.ResponseMeta.Usage.TotalTokens != total {
			t.Fatalf("inconsistent usage was silently repaired: total=%d answer=%+v err=%v", total, answer, err)
		}
	}
}
