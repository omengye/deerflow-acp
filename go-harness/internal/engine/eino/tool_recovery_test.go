package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func recoveryToolCall(id, value string) *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: fmt.Sprintf(`{"value":%q}`, value)}}})})
}

func TestAgentControlsFailureRetriesBeyondProgressThreshold(t *testing.T) {
	const failures = 6
	var attempts atomic.Int32
	underlying := &receiptEffectTool{run: func(context.Context) (string, error) {
		if attempts.Add(1) <= failures {
			return "read could not obtain a response", harness.MarkToolNoEffect(errors.New("service temporarily unavailable"))
		}
		return "available", nil
	}}
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call > 0 && call <= failures {
			seen := false
			for _, message := range input {
				seen = seen || message.Role == schema.Tool && message.ToolCallID == fmt.Sprintf("attempt-%d", call-1) && strings.Contains(message.Content, "service temporarily unavailable")
			}
			if !seen {
				t.Errorf("model did not receive failed attempt %d", call)
			}
		}
		if call <= failures {
			return recoveryToolCall(fmt.Sprintf("attempt-%d", call), "read"), nil
		}
		return textStream("Resolved after the service recovered."), nil
	}}
	s, session, _ := runtimeWithReceiptModel(t, underlying, fake)
	result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "read"}}, nil, approveReceiptTool)
	if err != nil || result.StopReason != "end_turn" || attempts.Load() != failures+1 || fake.calls != failures+2 {
		t.Fatalf("fixed retry cutoff or host replay: result=%+v err=%v attempts=%d models=%d", result, err, attempts.Load(), fake.calls)
	}
	receipts, err := s.ListToolReceipts(context.Background(), "owner", session.ID)
	if err != nil || len(receipts) != failures+1 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	for _, receipt := range receipts {
		if receipt.ToolCallID == fmt.Sprintf("attempt-%d", failures) {
			if receipt.State != harness.ReceiptCompleted || receipt.AgentRecoverable {
				t.Fatalf("successful final attempt=%+v", receipt)
			}
		} else if receipt.State != harness.ReceiptNoEffect || !receipt.AgentRecoverable {
			t.Fatalf("failed attempt evidence changed: %+v", receipt)
		}
	}
}

func TestToolFailureAsksInConversationAndUserReplyStartsFreshTurn(t *testing.T) {
	item := &correctingReceiptTool{first: errors.New("target environment is missing")}
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return recoveryToolCall("missing-environment", "unknown"), nil
		case 1:
			return textStream("Which environment should I use?"), nil
		case 2:
			assertClosedToolHistory(t, input)
			question, answer, failure := false, false, false
			for _, message := range input {
				question = question || message.Role == schema.Assistant && strings.Contains(message.Content, "Which environment")
				answer = answer || message.Role == schema.User && message.Content == "Use staging"
				failure = failure || message.Role == schema.Tool && message.ToolCallID == "missing-environment" && strings.Contains(message.Content, "effects are unconfirmed")
			}
			if !question || !answer || !failure {
				t.Errorf("fresh turn lost conversation: question=%v answer=%v failure=%v", question, answer, failure)
			}
			return recoveryToolCall("staging-request", "staging"), nil
		default:
			return textStream("Staging request complete."), nil
		}
	}}
	s, session, _ := runtimeWithReceiptModel(t, item, fake)
	ctx := context.Background()
	first, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "run the request"}}, nil, approveReceiptTool)
	if err != nil || first.StopReason != "end_turn" || len(item.args) != 1 || fake.calls != 2 {
		t.Fatalf("question did not wait for a normal answer: result=%+v err=%v args=%v", first, err, item.args)
	}
	before, err := s.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(before) != 1 || before[0].State != harness.ReceiptUncertain || !before[0].AgentRecoverable {
		t.Fatalf("failure=%+v err=%v", before, err)
	}
	second, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "Use staging"}}, nil, approveReceiptTool)
	if err != nil || second.StopReason != "end_turn" || len(item.args) != 2 || item.args[1] != `{"value":"staging"}` {
		t.Fatalf("normal reply failed or replayed old call: result=%+v err=%v args=%v", second, err, item.args)
	}
	after, err := s.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(after) != 2 {
		t.Fatalf("receipts=%+v err=%v", after, err)
	}
	for _, receipt := range after {
		if receipt.ToolCallID == "missing-environment" && (receipt.Version != before[0].Version || receipt.State != harness.ReceiptUncertain || receipt.Review != nil) {
			t.Fatalf("user's business answer changed execution evidence: %+v", receipt)
		}
		if receipt.ToolCallID == "staging-request" && receipt.RunID == before[0].RunID {
			t.Fatal("user reply resumed the old tool run")
		}
	}
}

func TestToolControlErrorsNeverBecomeAgentRecovery(t *testing.T) {
	for _, failure := range []error{
		&budgetError{resource: "tool_calls"},
		&durablebudget.LimitError{Resource: "tokens"},
		&durablebudget.PersistenceError{Operation: "fixture", Err: errors.New("database unavailable")},
		durablebudget.ErrScope,
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return recoveryToolCall("control-error", "read"), nil
				}
				t.Error("model continued after host control failure")
				return textStream("unexpected"), nil
			}}
			underlying := &receiptEffectTool{run: func(context.Context) (string, error) { return "", failure }}
			e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}})
			var ended *harness.ToolReceipt
			result, err := e.Run(context.Background(), request("control-error"), func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == "tool_end" {
					ended = event.Receipt
				}
				return nil
			}, allowTool)
			if err == nil && result.StopReason == "end_turn" || fake.calls != 1 || ended == nil || ended.AgentRecoverable {
				t.Fatalf("control failure hidden: result=%+v err=%v calls=%d receipt=%+v", result, err, fake.calls, ended)
			}
		})
	}
}

func TestToolTerminalReceiptFailureCannotBeReturnedToModel(t *testing.T) {
	failure := errors.New("terminal receipt write failed")
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return recoveryToolCall("storage-error", "read"), nil
		}
		t.Error("model continued without a durable terminal receipt")
		return textStream("unexpected"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{&receiptEffectTool{run: func(context.Context) (string, error) { return "partial", errors.New("transport error") }}}})
	_, err := e.Run(context.Background(), request("storage-error"), func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "tool_end" {
			return failure
		}
		return nil
	}, allowTool)
	if !errors.Is(err, failure) || fake.calls != 1 {
		t.Fatalf("storage failure hidden: err=%v calls=%d", err, fake.calls)
	}
}

func TestStreamingFailureReachesModelAfterAllTerminalEvidence(t *testing.T) {
	underlying := &receiptStreamTool{firstErr: errors.New("connection lost"), lateErr: errors.New("cleanup outcome unknown")}
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return recoveryToolCall("stream-error", "read"), nil
		}
		seen := false
		for _, message := range input {
			if message.Role == schema.Tool && strings.Contains(message.Content, "partial output") && strings.Contains(message.Content, underlying.firstErr.Error()) && strings.Contains(message.Content, underlying.lateErr.Error()) && strings.Contains(message.Content, "effects are unconfirmed") {
				seen = true
			}
		}
		if !seen {
			t.Error("model did not receive the final stream failure evidence")
		}
		return textStream("The result is unconfirmed; this attempt is ended."), nil
	}}
	s, session, _ := runtimeWithReceiptModel(t, underlying, fake)
	result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "read"}}, nil, approveReceiptTool)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, fake.calls)
	}
}

type enhancedRecoveryEvidence struct {
	cause  error
	result *schema.ToolResult
}

func (e *enhancedRecoveryEvidence) Error() string                          { return e.cause.Error() }
func (e *enhancedRecoveryEvidence) Unwrap() error                          { return e.cause }
func (e *enhancedRecoveryEvidence) EnhancedToolResult() *schema.ToolResult { return e.result }

type enhancedRecoveryFixture struct {
	result *schema.ToolResult
	err    error
}

func (*enhancedRecoveryFixture) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&recordingTool{}).Info(ctx)
}

type enhancedRecoveryInvoke struct{ enhancedRecoveryFixture }

func (f *enhancedRecoveryInvoke) InvokableRun(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.ToolResult, error) {
	return nil, &enhancedRecoveryEvidence{cause: f.err, result: f.result}
}

type enhancedRecoveryStream struct {
	enhancedRecoveryFixture
	late bool
}

func (f *enhancedRecoveryStream) StreamableRun(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	if !f.late {
		return nil, &enhancedRecoveryEvidence{cause: f.err, result: f.result}
	}
	reader, writer := schema.Pipe[*schema.ToolResult](1)
	go func() {
		defer writer.Close()
		writer.Send(f.result, nil)
		writer.Send(nil, f.err)
	}()
	return reader, nil
}

type immediateRecoveryStream struct{ err error }

func (*immediateRecoveryStream) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&recordingTool{}).Info(ctx)
}

func (f *immediateRecoveryStream) StreamableRun(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
	return nil, &receiptResultError{cause: f.err, result: "partial diagnostic"}
}

func TestToolFailureAdaptersPreserveEvidenceForModel(t *testing.T) {
	fixture := enhancedRecoveryFixture{result: &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "partial diagnostic"}}}, err: errors.New("remote outcome unknown")}
	for name, underlying := range map[string]tool.BaseTool{
		"enhanced_invoke":       &enhancedRecoveryInvoke{fixture},
		"enhanced_stream_start": &enhancedRecoveryStream{enhancedRecoveryFixture: fixture},
		"enhanced_stream_late":  &enhancedRecoveryStream{enhancedRecoveryFixture: fixture, late: true},
		"stream_start":          &immediateRecoveryStream{fixture.err},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return recoveryToolCall("adapter-error", "read"), nil
				}
				body, _ := json.Marshal(input)
				for _, evidence := range []string{"partial diagnostic", fixture.err.Error(), "effects are unconfirmed"} {
					if !strings.Contains(string(body), evidence) {
						t.Errorf("model lost %q", evidence)
					}
				}
				return textStream("The evidence is insufficient; ending this attempt."), nil
			}}
			s, session, _ := runtimeWithReceiptModel(t, underlying, fake)
			result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "read"}}, nil, approveReceiptTool)
			if err != nil || result.StopReason != "end_turn" || fake.calls != 2 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, fake.calls)
			}
			receipts, err := s.ListToolReceipts(context.Background(), "owner", session.ID)
			if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain || !receipts[0].AgentRecoverable {
				t.Fatalf("receipts=%+v err=%v", receipts, err)
			}
			body, _ := json.Marshal(receipts[0].Result)
			if !strings.Contains(string(body), "partial diagnostic") {
				t.Fatal("persisted receipt lost diagnostic evidence")
			}
		})
	}
}

func TestLocalToolCancellationDoesNotCancelOwningRun(t *testing.T) {
	underlying := &receiptStreamTool{firstErr: context.Canceled}
	fake := &scriptedModel{stream: func(ctx context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if ctx.Err() != nil {
			t.Errorf("local tool cancellation propagated to model context: %v", ctx.Err())
		}
		if call == 0 {
			return recoveryToolCall("local-cancel", "read"), nil
		}
		seen := false
		for _, message := range input {
			seen = seen || message.Role == schema.Tool && strings.Contains(message.Content, context.Canceled.Error()) && strings.Contains(message.Content, "partial output") && strings.Contains(message.Content, "effects are unconfirmed")
		}
		if !seen {
			t.Error("local cancellation lost its tool evidence")
		}
		return textStream("The remote operation was cancelled; this attempt is ended."), nil
	}}
	s, session, _ := runtimeWithReceiptModel(t, underlying, fake)
	result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "read"}}, nil, approveReceiptTool)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 2 {
		t.Fatalf("local cancel stopped owning run: result=%+v err=%v calls=%d", result, err, fake.calls)
	}
	receipts, err := s.ListToolReceipts(context.Background(), "owner", session.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain || !receipts[0].AgentRecoverable {
		t.Fatalf("local cancellation falsely certified: receipts=%+v err=%v", receipts, err)
	}
}

func TestNativeCancellationControlsRemainUnrecoverable(t *testing.T) {
	for _, control := range []error{&adk.CancelError{}, adk.ErrStreamCanceled, &adk.InterruptError{}, &durablebudget.PersistenceError{Operation: "cancelled write", Err: context.Canceled}} {
		if recoverableToolFailure(context.Background(), control) {
			t.Errorf("native control marker became recoverable: %T", control)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if recoverableToolFailure(ctx, errors.New("tool transport stopped")) {
		t.Error("cancelled owning context entered agent recovery")
	}
}
