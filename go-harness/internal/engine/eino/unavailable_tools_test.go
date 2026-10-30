package eino

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func unavailableToolCalls(names ...string) *schema.StreamReader[*schema.Message] {
	calls := make([]schema.ToolCall, 0, len(names))
	for _, name := range names {
		calls = append(calls, schema.ToolCall{ID: name, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: `{}`}})
	}
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", calls)})
}

func assertUnavailableFeedback(t *testing.T, messages []*schema.Message, names ...string) {
	t.Helper()
	assertClosedToolHistory(t, messages)
	for _, name := range names {
		found := false
		for _, message := range messages {
			if message.Role == schema.Tool && message.ToolCallID == name && strings.Contains(message.Content, "Tool failed:") && strings.Contains(message.Content, "no operation was performed") && strings.Contains(message.Content, "ordinary assistant message") {
				found = true
			}
		}
		if !found {
			t.Errorf("missing unavailable-tool feedback for %s", name)
		}
	}
}

func TestUnavailableToolMixedBatchAndConversationalFollowup(t *testing.T) {
	var effects, approvals, dispatched atomic.Int32
	fake := &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return unavailableToolCalls("read_workspace", "ask_user_question"), nil
		case 1:
			assertUnavailableFeedback(t, messages, "ask_user_question")
			return textStream("Which environment should I use?"), nil
		default:
			assertUnavailableFeedback(t, messages, "ask_user_question")
			question, answer := false, false
			for _, message := range messages {
				question = question || message.Role == schema.Assistant && message.Content == "Which environment should I use?"
				answer = answer || message.Role == schema.User && message.Content == "Use staging"
			}
			if !question || !answer {
				t.Errorf("followup lost conversation: question=%v answer=%v", question, answer)
			}
			return textStream("I will use staging."), nil
		}
	}}
	s, session, native := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) {
		effects.Add(1)
		return "workspace read", nil
	}}, false, harness.PermissionModeAll)
	approve := func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals.Add(1)
		if request.ToolName != "read_workspace" {
			t.Errorf("nonexistent tool requested permission: %+v", request)
		}
		return harness.AllowOnce, nil
	}
	emit := func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "tool_execute" && event.ToolName == "ask_user_question" {
			dispatched.Add(1)
		}
		return nil
	}
	ctx := context.Background()
	first, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "inspect"}}, emit, approve)
	if err != nil || first.StopReason != "end_turn" || fake.calls != 2 || effects.Load() != 1 || approvals.Load() != 1 || dispatched.Load() != 0 {
		t.Fatalf("result=%+v err=%v calls=%d effects=%d approvals=%d dispatched=%d", first, err, fake.calls, effects.Load(), approvals.Load(), dispatched.Load())
	}
	before, err := s.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(before) != 2 {
		t.Fatalf("receipts=%+v err=%v", before, err)
	}
	for _, receipt := range before {
		if receipt.ToolName == "ask_user_question" && (receipt.State != harness.ReceiptNotExecuted || !receipt.AgentRecoverable) || receipt.ToolName == "read_workspace" && receipt.State != harness.ReceiptCompleted {
			t.Fatalf("incorrect execution evidence: %+v", receipt)
		}
	}
	var tools int
	if err = native.DB().QueryRow(`SELECT tool_calls FROM budget_roots WHERE root_run_id=?`, first.Execution.RunID).Scan(&tools); err != nil || tools != 2 {
		t.Fatalf("tool budget=%d err=%v", tools, err)
	}
	second, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "Use staging"}}, emit, approve)
	if err != nil || second.StopReason != "end_turn" || first.Execution.RunID == second.Execution.RunID || fake.calls != 3 || effects.Load() != 1 || approvals.Load() != 1 {
		t.Fatalf("followup replayed work: result=%+v err=%v calls=%d effects=%d approvals=%d", second, err, fake.calls, effects.Load(), approvals.Load())
	}
	after, err := s.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("followup changed receipts: %+v err=%v", after, err)
	}
}

func TestUnavailableToolsRespectRoleAndHostFiltering(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "configured-role"}[child], func(t *testing.T) {
			names := []string{"ask_user_question", "task", "task_wait", "invoke_acp_agent", "skill_edit", "read_workspace"}
			var effects, approvals, rejected, dispatched atomic.Int32
			fake := &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if child {
					if call == 0 {
						return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}}})}), nil
					}
					call--
				}
				if call == 0 {
					return unavailableToolCalls(names...), nil
				}
				if call == 1 {
					assertUnavailableFeedback(t, messages, names...)
				}
				return textStream("The required tool is unavailable."), nil
			}}
			config := Config{ChatModel: fake, Tools: []tool.BaseTool{&receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "unexpected", nil }}}, PermissionMode: harness.PermissionModeAll, DisableSubAgent: !child}
			if child {
				config.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "inspect", Tools: []string{"write_todos"}}}}
			} else {
				config.ToolPolicy = harness.ToolPolicy{Denylist: []string{"read_workspace"}}
			}
			e, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			result, err := e.Run(context.Background(), request("unavailable-filter"), func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == "tool_execute" && event.ToolCallID != "delegate" {
					dispatched.Add(1)
				}
				if event.Kind == "tool_end" && event.ToolCallID != "delegate" {
					if event.Receipt == nil || event.Receipt.State != harness.ReceiptNotExecuted || !event.Receipt.AgentRecoverable {
						t.Errorf("unavailable call lost rejection evidence: %+v", event)
					}
					rejected.Add(1)
				}
				return nil
			}, func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals.Add(1)
				if !child || request.ToolCallID != "delegate" {
					t.Errorf("unavailable tool requested permission: %+v", request)
				}
				return harness.AllowOnce, nil
			})
			wantApprovals, wantCalls := int32(0), 2
			if child {
				wantApprovals, wantCalls = 1, 4
			}
			if err != nil || result.StopReason != "end_turn" || fake.calls != wantCalls || effects.Load() != 0 || approvals.Load() != wantApprovals || dispatched.Load() != 0 || rejected.Load() != int32(len(names)) {
				t.Fatalf("result=%+v err=%v calls=%d effects=%d approvals=%d dispatched=%d rejected=%d", result, err, fake.calls, effects.Load(), approvals.Load(), dispatched.Load(), rejected.Load())
			}
		})
	}
}

func TestUnavailableToolCannotHideHostFailures(t *testing.T) {
	for _, quota := range []bool{false, true} {
		t.Run(map[bool]string{false: "receipt-write", true: "budget"}[quota], func(t *testing.T) {
			fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				return unavailableToolCalls("ask_user_question"), nil
			}}
			config := Config{ChatModel: fake}
			if quota {
				config.Budget.MaxToolCalls = 1
			}
			e := newTestEngine(t, config)
			failure := errors.New("receipt write failed")
			result, err := e.Run(context.Background(), request("unknown-host-failure"), func(_ context.Context, event harness.RunEvent) error {
				if !quota && event.Kind == "tool_end" {
					return failure
				}
				return nil
			}, allowTool)
			if quota {
				if err != nil || result.Limit != "tool_calls" || fake.calls != 2 {
					t.Fatalf("budget failure hidden: result=%+v err=%v calls=%d", result, err, fake.calls)
				}
			} else if !errors.Is(err, failure) || fake.calls != 1 {
				t.Fatalf("receipt failure hidden: err=%v calls=%d", err, fake.calls)
			}
		})
	}
}
