package eino

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func delegateReaderMessage() *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}}}}})
}

func TestConfiguredRoleFailureReturnsToParentAfterJoining(t *testing.T) {
	for _, tc := range []string{"provider-start", "provider-stream", "provider-after-tool", "role-turn-limit"} {
		t.Run(tc, func(t *testing.T) {
			var stopped atomic.Bool
			failure := errors.New("fixture upstream unavailable")
			underlying := &recordingTool{}
			fake := &scriptedModel{stream: func(ctx context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return delegateReaderMessage(), nil
				}
				if call == 1 && (tc == "provider-after-tool" || tc == "role-turn-limit") {
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "known-effect", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
				}
				if call == 1 || call == 2 && tc == "provider-after-tool" {
					if tc != "provider-stream" {
						stopped.Store(true)
						return nil, failure
					}
					r, w := schema.Pipe[*schema.Message](1)
					go func() {
						defer w.Close()
						w.Send(&schema.Message{Role: schema.Assistant, Content: "partial"}, nil)
						w.Send(nil, failure)
						// The native graph can terminate after the first error. The
						// parent must still wait for cooperative provider teardown.
						time.Sleep(25 * time.Millisecond)
						w.Send(nil, errors.New("late provider read error"))
						stopped.Store(true)
					}()
					return r, nil
				}
				if tc != "role-turn-limit" && !stopped.Load() {
					t.Error("parent continued before child provider joined")
				}
				found := false
				for _, m := range input {
					if m.Role == schema.Tool && strings.Contains(m.Content, "completed tool receipts remain authoritative") {
						found = true
					}
				}
				if !found {
					t.Error("parent did not receive child failure result")
				}
				return textStream("parent handled child failure"), nil
			}}
			role := harness.AgentConfig{Description: "reader", SystemPrompt: "read", Tools: []string{"read_workspace"}}
			if tc == "role-turn-limit" {
				role.MaxTurns = rolePointer(1)
			}
			e, err := New(context.Background(), Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": role}}})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var events []harness.RunEvent
			result, err := e.Run(context.Background(), request(tc), func(_ context.Context, event harness.RunEvent) error {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, event)
				return nil
			}, allowTool)
			if err != nil || result.StopReason != "end_turn" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			wantEffects := int32(0)
			if tc == "provider-after-tool" || tc == "role-turn-limit" {
				wantEffects = 1
			}
			if underlying.calls.Load() != wantEffects {
				t.Fatalf("tool calls=%d want=%d", underlying.calls.Load(), wantEffects)
			}
			var failed, known int
			for _, event := range events {
				if event.Kind == "subagent_end" && event.Status == "failed" {
					failed++
				}
				if event.Kind == "tool_end" && event.ToolCallID == "known-effect" && event.Status == "completed" {
					known++
				}
			}
			if failed != 1 || known != int(wantEffects) {
				t.Fatalf("failed delegation=%d completed receipts=%d", failed, known)
			}
		})
	}
}

func TestConfiguredRoleUncertainToolStillStopsParent(t *testing.T) {
	for _, effectErr := range []error{errors.New("effect was started but confirmation was lost"), harness.ErrCommandUncertain, errors.Join(harness.MarkToolNotExecuted(errors.New("preflight failed")), errors.New("receipt storage failed"))} {
		t.Run(fmt.Sprint(effectErr), func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return delegateReaderMessage(), nil
				}
				if call == 1 {
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "unknown-effect", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
				}
				t.Error("parent or child continued after uncertain tool failure")
				return textStream("unexpected"), nil
			}}
			underlying := &receiptEffectTool{run: func(context.Context) (string, error) { return "", effectErr }}
			e, err := New(context.Background(), Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "reader", SystemPrompt: "read"}}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.Run(context.Background(), request("unknown-child-effect"), nil, allowTool)
			if !errors.Is(err, effectErr) {
				t.Fatalf("uncertain error lost: %v", err)
			}
		})
	}
}

func TestConfiguredRoleUsagePersistenceErrorCannotBecomeFailureResult(t *testing.T) {
	failure := errors.New("usage persistence failed")
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return delegateReaderMessage(), nil
		}
		if call == 1 {
			return nil, errors.New("upstream unavailable")
		}
		t.Error("parent continued after lost usage write")
		return textStream("unexpected"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "reader", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Run(context.Background(), request("usage-failure"), func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "usage" && fake.calls > 1 {
			return failure
		}
		return nil
	}, allowTool)
	if !errors.Is(err, failure) {
		t.Fatalf("storage failure lost: %v", err)
	}
}
