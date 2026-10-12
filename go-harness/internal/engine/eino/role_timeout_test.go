package eino

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestUnlimitedRunAndRoleHaveNoDeadlineAndRemainCancellable(t *testing.T) {
	for _, scenario := range []string{"default", "explicit-zero", "cancel-child"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Bound the test externally without hiding an injected task deadline.
			watchdog := time.AfterFunc(10*time.Second, cancel)
			defer watchdog.Stop()
			var childSeen, childJoined atomic.Bool
			fake := &scriptedModel{stream: func(callCtx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if deadline, ok := callCtx.Deadline(); ok {
					t.Errorf("model call %d unexpectedly bounded by %v", call, deadline)
				}
				if call == 0 {
					return delegateReaderMessage(), nil
				}
				if call == 1 {
					childSeen.Store(true)
					if scenario == "cancel-child" {
						cancel()
						<-callCtx.Done()
						childJoined.Store(true)
						return nil, callCtx.Err()
					}
					childJoined.Store(true)
					return textStream("child completed"), nil
				}
				if !childJoined.Load() {
					t.Error("parent continued before child joined")
				}
				return textStream("parent completed"), nil
			}}
			role := harness.AgentConfig{Description: "reader", SystemPrompt: "read"}
			if scenario == "explicit-zero" {
				role.TimeoutSeconds = rolePointer(0.0)
			}
			e, err := New(ctx, Config{ChatModel: fake, Budget: harness.DefaultBudgetLimits(), Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": role}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := e.Run(ctx, request(scenario), nil, allowTool)
			if !childSeen.Load() || !childJoined.Load() {
				t.Fatalf("child did not execute and join: %+v %v", result, err)
			}
			if scenario == "cancel-child" {
				if result.StopReason != "cancelled" {
					t.Fatalf("unlimited child ignored cancellation: %+v %v", result, err)
				}
			} else if err != nil || result.StopReason != "end_turn" {
				t.Fatalf("unlimited run did not complete: %+v %v", result, err)
			}
		})
	}
}

func TestConfiguredRoleTimeoutAndCancellationBoundaries(t *testing.T) {
	for _, scenario := range []string{"model-timeout", "stream-timeout", "safe-tool-timeout", "unknown-tool-timeout", "user-cancel", "storage-failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var joined, continued, usedTool atomic.Bool
			storageErr := errors.New("fixture usage write failed")
			usesTool := strings.Contains(scenario, "tool-timeout")
			fake := &scriptedModel{stream: func(childCtx context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return delegateReaderMessage(), nil
				}
				if call == 1 {
					if usesTool {
						return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "timed-effect", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
					}
					if scenario == "user-cancel" {
						cancel()
					}
					if scenario == "stream-timeout" {
						r, w := schema.Pipe[*schema.Message](1)
						go func() {
							defer w.Close()
							w.Send(&schema.Message{Role: schema.Assistant, Content: "partial"}, nil)
							<-childCtx.Done()
							w.Send(nil, childCtx.Err())
							time.Sleep(20 * time.Millisecond)
							joined.Store(true)
						}()
						return r, nil
					}
					<-childCtx.Done()
					time.Sleep(20 * time.Millisecond)
					joined.Store(true)
					return nil, childCtx.Err()
				}
				continued.Store(true)
				if !joined.Load() {
					t.Error("parent resumed before real child cleanup joined")
				}
				found := false
				for _, message := range input {
					if message.Role == schema.Tool && strings.Contains(message.Content, "exceeded its time limit") {
						found = true
					}
				}
				if !found {
					t.Error("parent did not receive the role timeout")
				}
				return textStream("parent recovered"), nil
			}}
			underlying := &receiptEffectTool{run: func(effectCtx context.Context) (string, error) {
				usedTool.Store(true)
				<-effectCtx.Done()
				time.Sleep(20 * time.Millisecond)
				joined.Store(true)
				if scenario == "safe-tool-timeout" {
					return "", harness.MarkToolNoEffect(effectCtx.Err())
				}
				return "", effectCtx.Err()
			}}
			e, err := New(ctx, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "reader", SystemPrompt: "read", TimeoutSeconds: rolePointer(0.3)}}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := e.Run(ctx, request(scenario), func(_ context.Context, event harness.RunEvent) error {
				if scenario == "storage-failure" && event.Kind == "usage" && joined.Load() {
					return storageErr
				}
				return nil
			}, allowTool)
			safe := scenario == "model-timeout" || scenario == "stream-timeout" || scenario == "safe-tool-timeout"
			if continued.Load() != safe || !joined.Load() || usesTool != usedTool.Load() {
				t.Fatalf("continued=%v joined=%v usedTool=%v result=%+v err=%v", continued.Load(), joined.Load(), usedTool.Load(), result, err)
			}
			if safe && (err != nil || result.StopReason != "end_turn") {
				t.Fatalf("safe role timeout failed parent: %+v %v", result, err)
			}
			if scenario == "storage-failure" && !errors.Is(err, storageErr) {
				t.Fatalf("lost persistence error: %v", err)
			}
			if scenario == "user-cancel" && result.StopReason != "cancelled" {
				t.Fatalf("explicit stop was swallowed: %+v %v", result, err)
			}
		})
	}
}
