package eino

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestCancelledToolAllowsFreshPromptWithoutReplayingEffect(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "durable"}[durable], func(t *testing.T) {
			started := make(chan struct{})
			var effects atomic.Int32
			underlying := &receiptEffectTool{run: func(ctx context.Context) (string, error) {
				if effects.Add(1) == 1 {
					close(started)
					<-ctx.Done()
					return "partial effect before stop", ctx.Err()
				}
				return "fresh effect", nil
			}}
			var warned atomic.Bool
			fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call > 0 {
					assertClosedToolHistory(t, input)
					for _, message := range input {
						if message.Role == schema.System && strings.Contains(message.Content, "stopped tool calls") {
							warned.Store(true)
						}
					}
				}
				if call == 0 || call == 2 {
					id := "stopped-call"
					if call == 2 {
						id = "fresh-call"
					}
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
				}
				return textStream("new answer"), nil
			}}
			var s *hr.Service
			var session harness.Session
			var native *sqlite.Store
			if durable {
				s, session, native = runtimeWithDurableInteraction(t, fake, underlying, false)
			} else {
				s, session, native = runtimeWithReceiptModel(t, underlying, fake)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			type outcome struct {
				result harness.RunResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "run until stopped"}}, nil, approveReceiptTool)
				done <- outcome{result, err}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("tool did not start")
			}
			if err := s.Cancel(ctx, "owner", session.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case stopped := <-done:
				if stopped.err != nil || stopped.result.StopReason != "cancelled" {
					t.Fatalf("stop=%+v err=%v", stopped.result, stopped.err)
				}
			case <-ctx.Done():
				t.Fatal("cancel did not join the tool")
			}
			receipts, err := s.ListToolReceipts(ctx, "owner", session.ID)
			if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain {
				t.Fatalf("stopped effect was falsely certified: %+v %v", receipts, err)
			}
			stopped := receipts[0]
			for _, prompt := range []string{"new question", "new tool request"} {
				result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, approveReceiptTool)
				if err != nil || result.StopReason != "end_turn" {
					t.Fatalf("fresh prompt %q blocked after stop: %+v %v", prompt, result, err)
				}
			}
			if effects.Load() != 2 || !warned.Load() {
				t.Fatalf("stopped work replayed or safety context lost: effects=%d warned=%v", effects.Load(), warned.Load())
			}
			receipts, err = s.ListToolReceipts(ctx, "owner", session.ID)
			if err != nil || len(receipts) != 2 {
				t.Fatalf("receipts=%+v err=%v", receipts, err)
			}
			for _, receipt := range receipts {
				if receipt.ToolCallID == stopped.ToolCallID && (receipt.State != harness.ReceiptUncertain || receipt.Version != stopped.Version || receipt.Review != nil) {
					t.Fatalf("new prompt rewrote old evidence: %+v", receipt)
				}
			}
			if durable {
				var checkpoints int
				if err := native.DB().QueryRow(`SELECT COUNT(*) FROM eino_checkpoints WHERE id=?`, CheckpointID(stopped.RunID)).Scan(&checkpoints); err != nil || checkpoints != 0 {
					t.Fatalf("cancelled checkpoint retained: count=%d err=%v", checkpoints, err)
				}
				_, err := s.ResumeExecution(ctx, "owner", session.ID, harness.ResumeExecutionRequest{RunID: stopped.RunID, ExpectedVersion: 1}, nil, approveReceiptTool)
				if !errors.Is(err, harness.ErrExecutionConflict) {
					t.Fatalf("stopped execution became resumable: %v", err)
				}
			}
		})
	}
}

func assertClosedToolHistory(t *testing.T, messages []*schema.Message) {
	t.Helper()
	pending := make(map[string]bool)
	for _, message := range messages {
		if message == nil {
			continue
		}
		if message.Role != schema.Tool && len(pending) > 0 {
			t.Errorf("new message follows unfinished tool history: pending=%v role=%s", pending, message.Role)
		}
		for _, call := range message.ToolCalls {
			pending[call.ID] = true
		}
		if message.Role == schema.Tool {
			if !pending[message.ToolCallID] {
				t.Errorf("orphaned tool result after cancellation: %s", message.ToolCallID)
			}
			delete(pending, message.ToolCallID)
		}
	}
	if len(pending) > 0 {
		t.Errorf("model request retains unfinished tool calls: %v", pending)
	}
}

func TestCancelledModelAllowsNextTurnWithFreshBudget(t *testing.T) {
	started := make(chan struct{})
	fake := &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call > 0 {
			return textStream("answer after stop"), nil
		}
		reader, writer := schema.Pipe[*schema.Message](1)
		go func() {
			defer writer.Close()
			writer.Send(schema.AssistantMessage("partial answer", nil), nil)
			close(started)
			<-ctx.Done()
			writer.Send(nil, ctx.Err())
		}()
		return reader, nil
	}}
	s, session, native := runtimeWithDurableInteraction(t, fake, &recordingTool{}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "start streaming"}}, nil, nil)
		if err == nil && result.StopReason != "cancelled" {
			err = errors.New("wrong cancellation result")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("model did not start")
	}
	if err := s.Cancel(ctx, "owner", session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("model cancellation did not settle")
	}
	result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "new question"}}, nil, nil)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("new prompt blocked: %+v %v", result, err)
	}
	var roots, active, held int
	if err := native.DB().QueryRow(`SELECT COUNT(*),SUM(held_tokens) FROM budget_roots`).Scan(&roots, &held); err != nil || roots != 2 || held != 0 {
		t.Fatalf("budget not settled/replaced: roots=%d held=%d err=%v", roots, held, err)
	}
	if err := native.DB().QueryRow(`SELECT COUNT(*) FROM budget_attempts WHERE state='active'`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("cancelled attempt remained active: count=%d err=%v", active, err)
	}
}
