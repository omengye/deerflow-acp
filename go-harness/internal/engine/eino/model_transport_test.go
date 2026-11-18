package eino

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func resetModelStream(prefix ...*schema.Message) *schema.StreamReader[*schema.Message] {
	reader, writer := schema.Pipe[*schema.Message](len(prefix) + 1)
	for _, message := range prefix {
		writer.Send(message, nil)
	}
	writer.Send(nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET})
	writer.Close()
	return reader
}

func TestModelTransportResetBeforeFirstChunkRetriesSameRequest(t *testing.T) {
	for _, mode := range []string{"generate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
					if call == 0 {
						return resetModelStream(), nil
					}
					return textStream("recovered"), nil
				}}
				m := rateLimitTracked(context.Background(), fake, harness.BudgetLimits{MaxModelCalls: 4, MaxOutputTokens: 64})
				start := time.Now()
				answer, err := callRateLimitTracked(context.Background(), m, mode)
				m.io.closeAndWait()
				if err != nil || answer != "recovered" || fake.calls != 2 || m.budget.modelCalls != 2 || m.budget.held != 0 || time.Since(start) != 2*time.Second {
					t.Fatalf("answer=%q err=%v calls=%d budget=%+v delay=%v", answer, err, fake.calls, m.budget, time.Since(start))
				}
			})
		})
	}
}

func TestModelTransportResetAfterOutputNeverReplays(t *testing.T) {
	for _, prefix := range []*schema.Message{
		schema.AssistantMessage("partial", nil),
		{Role: schema.Assistant, ReasoningContent: "partial reasoning"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "partial-tool", Function: schema.FunctionCall{Name: "execute", Arguments: "{"}}}},
	} {
		for _, mode := range []string{"generate", "stream"} {
			synctest.Test(t, func(t *testing.T) {
				fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
					return resetModelStream(prefix), nil
				}}
				m := rateLimitTracked(context.Background(), fake, harness.BudgetLimits{MaxModelCalls: 8})
				_, err := callRateLimitTracked(context.Background(), m, mode)
				m.io.closeAndWait()
				if fake.calls != 1 || !errors.Is(err, harness.ErrModelUnavailable) || retryableModelRateLimit(err) {
					t.Fatalf("partial %s replayed or unclassified: calls=%d err=%v", mode, fake.calls, err)
				}
			})
		}
	}
}

func TestModelTransportRetryKeepsCompletedTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := &recordingTool{}
		fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
			if call == 0 {
				return limitedOutput("", "tool_calls", schema.ToolCall{ID: "completed-call", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"preserved"}`}}), nil
			}
			found := false
			for _, message := range input {
				found = found || message.Role == schema.Tool && message.ToolCallID == "completed-call" && strings.Contains(message.Content, "preserved")
			}
			if !found || target.calls.Load() != 1 {
				t.Errorf("completed tool lost or replayed: found=%v effects=%d", found, target.calls.Load())
			}
			if call == 1 {
				return resetModelStream(), nil
			}
			return limitedOutput("completed", "stop"), nil
		}}
		e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{target}})
		result, err := e.Run(context.Background(), request("stream-reset"), nil, allowTool)
		if err != nil || result.StopReason != "end_turn" || fake.calls != 3 || target.calls.Load() != 1 {
			t.Fatalf("result=%+v err=%v calls=%d tools=%d", result, err, fake.calls, target.calls.Load())
		}
	})
}
