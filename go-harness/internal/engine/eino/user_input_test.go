package eino

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func questionCall() *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: "question", Type: "function", Function: schema.FunctionCall{Name: "ask_user_question", Arguments: `{"questions":[{"header":"Environment","question":"Which environment?","options":[{"label":"staging"},{"label":"production"}]},{"header":"Notes","question":"Any deployment notes?","options":[]}]}`}}})})
}

func questionReply(req harness.UserInputRequest) harness.UserInputResponse {
	return harness.UserInputResponse{Outcome: "accepted", Answers: []harness.UserInputAnswer{
		{QuestionID: req.Questions[0].ID, Answers: []string{"staging"}},
		{QuestionID: req.Questions[1].ID, Answers: []string{"Deploy after the backup"}},
	}}
}

func assertQuestionAnswer(t *testing.T, messages []*schema.Message) {
	t.Helper()
	assertClosedToolHistory(t, messages)
	for _, message := range messages {
		if message.Role == schema.Tool && message.ToolCallID == "question" && strings.Contains(message.Content, "staging") && strings.Contains(message.Content, "Deploy after the backup") {
			return
		}
	}
	t.Error("model did not receive selected and free-text user answers")
}

func TestAskUserQuestionDoesNotRequireToolApproval(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "all-tools", true: "read-only"}[readOnly], func(t *testing.T) {
			var questions, approvals atomic.Int32
			fake := &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return questionCall(), nil
				}
				assertQuestionAnswer(t, messages)
				return textStream("I will use staging after the backup."), nil
			}}
			e := newTestEngine(t, Config{ChatModel: fake, PermissionMode: harness.PermissionModeAll})
			req := request("user-question")
			if readOnly {
				req.Session.Mode, req.Session.ApprovalMode = "plan", harness.ApprovalReadOnly
			}
			ctx := interaction.WithUserInput(context.Background(), func(_ context.Context, input harness.UserInputRequest) (harness.UserInputResponse, error) {
				questions.Add(1)
				if input.SessionID != req.Session.ID || input.RunID != req.RunID || input.ToolCallID != "question" {
					t.Errorf("wrong question identity: %+v", input)
				}
				return questionReply(input), nil
			})
			result, err := e.Run(ctx, req, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals.Add(1)
				return harness.RejectOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" || questions.Load() != 1 || approvals.Load() != 0 || fake.calls != 2 {
				t.Fatalf("result=%+v err=%v questions=%d approvals=%d calls=%d", result, err, questions.Load(), approvals.Load(), fake.calls)
			}
		})
	}
}

func TestAskUserQuestionCapabilityAndExternalIdentity(t *testing.T) {
	t.Run("unsupported-client", func(t *testing.T) {
		fake := &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
			if call == 0 {
				return questionCall(), nil
			}
			assertUnavailableFeedback(t, messages, "question")
			return textStream("Which environment?"), nil
		}}
		e := newTestEngine(t, Config{ChatModel: fake})
		result, err := e.Run(context.Background(), request("unsupported-question"), nil, nil)
		if err != nil || result.StopReason != "end_turn" || fake.calls != 2 {
			t.Fatalf("unsupported client failed: result=%+v err=%v calls=%d", result, err, fake.calls)
		}
		for _, names := range fake.tools {
			if slices.Contains(names, "ask_user_question") {
				t.Fatalf("unsupported client advertised user input: %v", names)
			}
		}
	})
	t.Run("external-same-name", func(t *testing.T) {
		var effects, approvals atomic.Int32
		fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
			if call == 0 {
				return questionCall(), nil
			}
			return textStream("Denied."), nil
		}}
		underlying := &namedEffectTool{receiptEffectTool: &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "changed", nil }}, name: "ask_user_question"}
		e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, PermissionMode: harness.PermissionModeAll})
		result, err := e.Run(context.Background(), request("external-question"), nil, func(_ context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
			approvals.Add(1)
			if req.TrustedBuiltin {
				t.Error("external tool acquired host question identity")
			}
			return harness.RejectOnce, nil
		})
		if err != nil || result.StopReason != "end_turn" || approvals.Load() != 1 || effects.Load() != 0 {
			t.Fatalf("result=%+v err=%v approvals=%d effects=%d", result, err, approvals.Load(), effects.Load())
		}
	})
}

func TestAskUserQuestionDurableAnswerDoesNotAuthorizeEffects(t *testing.T) {
	var effects, approvals atomic.Int32
	fake := &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return questionCall(), nil
		case 1:
			assertQuestionAnswer(t, messages)
			return recoveryToolCall("effect", "staging"), nil
		default:
			assertClosedToolHistory(t, messages)
			return textStream("The operation still requires its own permission."), nil
		}
	}}
	s, session, native := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "changed", nil }}, false, harness.PermissionModeAll)
	ctx := interaction.WithUserInput(context.Background(), func(_ context.Context, req harness.UserInputRequest) (harness.UserInputResponse, error) {
		return questionReply(req), nil
	})
	result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "prepare deployment"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals.Add(1)
		if p.ToolName != "read_workspace" {
			t.Errorf("question requested tool permission: %+v", p)
		}
		return harness.RejectOnce, nil
	})
	if err != nil || result.StopReason != "end_turn" || approvals.Load() != 1 || effects.Load() != 0 || fake.calls != 3 {
		t.Fatalf("result=%+v err=%v approvals=%d effects=%d calls=%d", result, err, approvals.Load(), effects.Load(), fake.calls)
	}
	receipts, err := s.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	for _, receipt := range receipts {
		if receipt.ToolName == "ask_user_question" && (receipt.State != harness.ReceiptCompleted || len(receipt.Result) == 0 || !strings.Contains(receipt.Result[0].Text, "staging")) {
			t.Fatalf("question result not persisted: %+v", receipt)
		}
	}
	var tools, held int
	if err := native.DB().QueryRow(`SELECT tool_calls,held_tokens FROM budget_roots WHERE root_run_id=?`, result.Execution.RunID).Scan(&tools, &held); err != nil || tools != 2 || held != 0 {
		t.Fatalf("budget tools=%d held=%d err=%v", tools, held, err)
	}
}

func TestAskUserQuestionCancellationAllowsFreshTurn(t *testing.T) {
	started := make(chan struct{})
	fake := &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return questionCall(), nil
		}
		assertClosedToolHistory(t, messages)
		return textStream("Continue without the cancelled question."), nil
	}}
	s, session, _ := runtimeWithDurableInteraction(t, fake, &recordingTool{}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = interaction.WithUserInput(ctx, func(ctx context.Context, _ harness.UserInputRequest) (harness.UserInputResponse, error) {
		close(started)
		<-ctx.Done()
		return harness.UserInputResponse{}, ctx.Err()
	})
	type outcome struct {
		result harness.RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "ask"}}, nil, nil)
		done <- outcome{result, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("question did not reach the client")
	}
	if err := s.Cancel(ctx, "owner", session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case ended := <-done:
		if ended.err != nil || ended.result.StopReason != "cancelled" {
			t.Fatalf("cancellation=%+v", ended)
		}
	case <-ctx.Done():
		t.Fatal("cancelled question did not join")
	}
	receipts, err := s.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptNoEffect {
		t.Fatalf("cancelled question has uncertain effects: receipts=%+v err=%v", receipts, err)
	}
	result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, nil)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 2 {
		t.Fatalf("followup=%+v err=%v calls=%d", result, err, fake.calls)
	}
}

func TestAskUserQuestionDelegatedRolesReturnQuestionsToRoot(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-child", true: "configured-role"}[configured], func(t *testing.T) {
			var questions, approvals, rejected atomic.Int32
			var fake *scriptedModel
			fake = &scriptedModel{stream: func(_ context.Context, call int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				switch call {
				case 0:
					role := "general-purpose"
					if configured {
						role = "reader"
					}
					return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"` + role + `","prompt":"confirm environment","description":"inspect"}`}}})}), nil
				case 1:
					fake.mu.Lock()
					names := append([]string(nil), fake.tools[len(fake.tools)-1]...)
					fake.mu.Unlock()
					if slices.Contains(names, "ask_user_question") {
						t.Errorf("child advertised the root question tool: %v", names)
					}
					return unavailableToolCalls("ask_user_question"), nil
				case 2:
					assertUnavailableFeedback(t, messages, "ask_user_question")
					if questions.Load() != 0 {
						t.Error("delegated role opened a desktop question")
					}
					return textStream("Ask the user which environment to use."), nil
				case 3:
					return questionCall(), nil
				default:
					assertQuestionAnswer(t, messages)
					return textStream("Use staging."), nil
				}
			}}
			config := Config{ChatModel: fake, PermissionMode: harness.PermissionModeAll}
			if configured {
				config.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "inspect", SystemPrompt: "confirm needed information", Tools: []string{"ask_user_question", "write_todos"}}}}
			}
			e, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			ctx := interaction.WithUserInput(context.Background(), func(_ context.Context, req harness.UserInputRequest) (harness.UserInputResponse, error) {
				questions.Add(1)
				return questionReply(req), nil
			})
			result, err := e.Run(ctx, request("role-question"), func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == "tool_end" && event.ToolCallID == "ask_user_question" {
					if event.Receipt == nil || event.Receipt.State != harness.ReceiptNotExecuted {
						t.Errorf("child question did not retain rejection evidence: %+v", event)
					}
					rejected.Add(1)
				}
				return nil
			}, func(_ context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals.Add(1)
				if req.ToolName != "task" {
					t.Errorf("role question requested tool permission: %+v", req)
				}
				return harness.AllowOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" || questions.Load() != 1 || approvals.Load() != 1 || rejected.Load() != 1 || fake.calls != 5 {
				t.Fatalf("result=%+v err=%v questions=%d approvals=%d rejected=%d calls=%d", result, err, questions.Load(), approvals.Load(), rejected.Load(), fake.calls)
			}
		})
	}
}
