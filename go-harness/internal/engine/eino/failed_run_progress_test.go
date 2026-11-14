package eino

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

// A long run that fails after completing tool work must not lose that work:
// the completed tool results stay in durable session context, so a follow-up
// prompt continues from them without re-executing the side effect.
func TestFailedRunKeepsCompletedToolProgressForFollowup(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "durable"}[durable], func(t *testing.T) {
			var effects atomic.Int32
			underlying := &receiptEffectTool{run: func(context.Context) (string, error) {
				effects.Add(1)
				return "progress-marker-42", nil
			}}
			var sawProgress atomic.Bool
			fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				switch call {
				case 0:
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "work-call", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
				case 1:
					return nil, &openaiapi.APIError{HTTPStatusCode: http.StatusBadRequest, Message: "unrecoverable provider rejection"}
				}
				assertClosedToolHistory(t, input)
				for _, message := range input {
					if message.Role == schema.Tool && strings.Contains(message.Content, "progress-marker-42") {
						sawProgress.Store(true)
					}
				}
				return textStream("continued"), nil
			}}
			var s *hr.Service
			var session harness.Session
			if durable {
				s, session, _ = runtimeWithDurableInteraction(t, fake, underlying, false)
			} else {
				s, session, _ = runtimeWithReceiptModel(t, underlying, fake)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "long task"}}, nil, approveReceiptTool); err == nil && result.StopReason == "end_turn" {
				t.Fatalf("provider rejection did not fail the run: %+v", result)
			}
			result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, approveReceiptTool)
			if err != nil || result.StopReason != "end_turn" {
				t.Fatalf("follow-up blocked after failure: %+v %v", result, err)
			}
			if !sawProgress.Load() || effects.Load() != 1 {
				t.Fatalf("completed progress lost or replayed: saw=%v effects=%d", sawProgress.Load(), effects.Load())
			}
		})
	}
}

// A process crash in the middle of a long run must not discard tool work that
// already completed. A fresh process over the same database continues from it.
func TestCrashedRunKeepsCompletedToolProgressAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.db")
	hung := make(chan struct{})
	release := make(chan struct{})
	var effects atomic.Int32
	effect := &receiptEffectTool{run: func(ctx context.Context) (string, error) {
		if effects.Add(1) == 1 {
			return "progress-marker-42", nil
		}
		close(hung)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", context.Canceled
	}}
	var sawProgress, warned atomic.Bool
	model := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call >= 2 {
			assertClosedToolHistory(t, input)
			for _, message := range input {
				if message.Role == schema.System && strings.Contains(message.Content, "stopped or failed tool calls") {
					warned.Store(true)
				}
			}
		}
		for _, message := range input {
			if message.Role == schema.Tool && strings.Contains(message.Content, "progress-marker-42") && message.ToolCallID == "first" {
				for _, m := range input {
					if m.Role == schema.User && m.Content == "continue" {
						sawProgress.Store(true)
					}
				}
			}
		}
		if call < 2 {
			id := []string{"first", "second"}[call]
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
		}
		return textStream("continued"), nil
	}}
	service := func() *hr.Service {
		// Each process owns its own database handle and in-memory session locks.
		native, err := sqlite.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { native.Close() })
		store, err := hr.NewStore(ctx, native.DB())
		if err != nil {
			t.Fatal(err)
		}
		ledger, err := durablebudget.New(native.DB(), durablebudget.Config{})
		if err != nil {
			t.Fatal(err)
		}
		limits := harness.BudgetLimits{MaxModelCalls: 10, MaxToolCalls: 10, MaxTokens: 100000, MaxOutputTokens: 100}
		store.BudgetLedger, store.BudgetLimits = ledger, limits
		e, err := New(ctx, Config{ChatModel: model, Tools: []tool.BaseTool{effect}, Budget: limits, BudgetLedger: ledger, SessionStore: native, CheckpointStore: native, DisableSubAgent: true})
		if err != nil {
			t.Fatal(err)
		}
		return hr.NewService(store, e, "test")
	}
	first := service()
	session, err := first.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	crashed, stop := context.WithCancel(ctx)
	go first.Run(crashed, "owner", session.ID, []harness.Content{{Type: "text", Text: "long task"}}, nil, approveReceiptTool)
	select {
	case <-hung:
	case <-time.After(10 * time.Second):
		t.Fatal("second tool did not start")
	}
	// The first process is gone: nothing it holds in memory may be relied on.
	defer func() { close(release); stop() }()
	restarted := service()
	if err := restarted.Store.ReconcileInterrupted(ctx); err != nil {
		t.Fatalf("startup reconciliation: %v", err)
	}
	if _, err := restarted.Load(ctx, "owner", session.ID, session.CWD, false, nil); err != nil {
		t.Fatalf("restarted process cannot load the crashed session: %v", err)
	}
	followup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := restarted.Run(followup, "owner", session.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, approveReceiptTool)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("follow-up after crash blocked: %+v %v", result, err)
	}
	if !sawProgress.Load() || effects.Load() != 2 || !warned.Load() {
		t.Fatalf("completed progress lost, replayed or unflagged: saw=%v effects=%d warned=%v", sawProgress.Load(), effects.Load(), warned.Load())
	}
}
