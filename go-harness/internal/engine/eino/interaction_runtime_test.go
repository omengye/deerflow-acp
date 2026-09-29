package eino

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func runtimeWithDurableInteraction(t *testing.T, fake *scriptedModel, underlying tool.BaseTool, subagents bool) (*hr.Service, harness.Session, *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "runtime.db"))
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
	e, err := New(ctx, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger, SessionStore: native, CheckpointStore: native, DisableSubAgent: !subagents})
	if err != nil {
		t.Fatal(err)
	}
	s := hr.NewService(store, e, "test")
	s.Settings.EnableSubagents, s.Settings.DefaultSubagents = subagents, subagents
	session, err := s.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, session, native
}

func TestRuntimeNativeDurablePermissionAllowAndDeny(t *testing.T) {
	for _, decision := range []harness.PermissionDecision{harness.AllowOnce, harness.RejectOnce} {
		t.Run(string(decision), func(t *testing.T) {
			var effects atomic.Int32
			underlying := &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "effect", nil }}
			s, session, native := runtimeWithDurableInteraction(t, toolScript(), underlying, false)
			var permissions atomic.Int32
			result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "run once"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				permissions.Add(1)
				return decision, nil
			})
			if err != nil || result.StopReason != "end_turn" {
				t.Fatalf("runtime native result=%+v err=%v", result, err)
			}
			want := int32(0)
			if decision == harness.AllowOnce {
				want = 1
			}
			if effects.Load() != want || permissions.Load() != 1 {
				t.Fatalf("effects=%d permission=%d", effects.Load(), permissions.Load())
			}
			var grants, attempts int
			if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_execution_grants WHERE state='consumed'`).Scan(&grants); err != nil {
				t.Fatal(err)
			}
			if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_execution_attempts`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if grants != 1 || attempts != 2 {
				t.Fatalf("grants=%d attempts=%d", grants, attempts)
			}
		})
	}
}

func TestRuntimeNativeNestedSubagentPermission(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"general-purpose","prompt":"inspect","description":"inspect"}`}}}}}), nil
		case 1:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "child-read", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
		default:
			return textStream("done"), nil
		}
	}}
	var effects atomic.Int32
	s, session, native := runtimeWithDurableInteraction(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "child effect", nil }}, true)
	trace := make(map[string]int)
	result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, func(_ context.Context, event harness.RunEvent) error { trace[event.Kind]++; return nil }, approveReceiptTool)
	if err != nil || result.StopReason != "end_turn" || effects.Load() != 1 {
		receipts, receiptErr := s.Store.ListToolReceipts(context.Background(), session.ID)
		var states []string
		for _, receipt := range receipts {
			states = append(states, receipt.ToolName+":"+string(receipt.State))
		}
		t.Fatalf("nested runtime result=%+v effects=%d err=%v receipts=%v receiptErr=%v", result, effects.Load(), err, states, receiptErr)
	}
	for _, kind := range []string{"subagent_start", "subagent_suspended", "subagent_resumed", "subagent_end"} {
		if trace[kind] != 1 {
			t.Fatalf("missing/duplicate delegation trace: %+v", trace)
		}
	}
	var toolCalls, modelCalls, receipts int
	if err = native.DB().QueryRow(`SELECT tool_calls,model_calls FROM budget_roots`).Scan(&toolCalls, &modelCalls); err != nil {
		t.Fatal(err)
	}
	if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_tool_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if toolCalls != 1 || modelCalls != 4 || receipts != 1 {
		t.Fatalf("delegation accounting tools=%d models=%d receipts=%d", toolCalls, modelCalls, receipts)
	}
}

func TestRuntimeNativeEventFailureStopsBeforeToolEffect(t *testing.T) {
	for _, failedKind := range []string{"tool_start", "tool_execute"} {
		t.Run(failedKind, func(t *testing.T) {
			want := errors.New("consumer rejected " + failedKind)
			var effects, permissions atomic.Int32
			fake := toolScript()
			s, session, native := runtimeWithDurableInteraction(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) {
				effects.Add(1)
				return "effect must not run", nil
			}}, false)
			result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "test callback failure"}}, func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == failedKind {
					return want
				}
				return nil
			}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				permissions.Add(1)
				return harness.AllowOnce, nil
			})
			if !errors.Is(err, want) || effects.Load() != 0 {
				t.Fatalf("callback failure allowed effect: result=%+v err=%v effects=%d", result, err, effects.Load())
			}
			fake.mu.Lock()
			modelCalls := fake.calls
			fake.mu.Unlock()
			wantPermissions := int32(0)
			if failedKind == "tool_execute" {
				wantPermissions = 1
			}
			if modelCalls != 1 || permissions.Load() != wantPermissions {
				t.Fatalf("execution continued after callback failure: modelCalls=%d permissions=%d", modelCalls, permissions.Load())
			}
			if result.Execution == nil || result.Execution.Status == harness.ExecutionCompleted {
				t.Fatalf("callback failure reported successful completion: %+v", result.Execution)
			}
			var ledgerModels, held int64
			if err = native.DB().QueryRow(`SELECT model_calls,held_tokens FROM budget_roots`).Scan(&ledgerModels, &held); err != nil {
				t.Fatal(err)
			}
			if ledgerModels != 1 || held != 0 {
				t.Fatalf("failed callback accounting: modelCalls=%d held=%d", ledgerModels, held)
			}
		})
	}
}
