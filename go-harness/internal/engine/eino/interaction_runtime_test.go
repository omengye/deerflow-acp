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
	return runtimeWithDurableInteractionMode(t, fake, underlying, subagents, "")
}

func runtimeWithDurableInteractionMode(t *testing.T, fake *scriptedModel, underlying tool.BaseTool, subagents bool, mode harness.PermissionMode) (*hr.Service, harness.Session, *sqlite.Store) {
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
	e, err := New(ctx, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger, SessionStore: native, CheckpointStore: native, DisableSubAgent: !subagents, PermissionMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	s := hr.NewService(store, e, "test")
	s.PermissionMode = mode
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

func TestRuntimePermissionModesGateSafeToolBeforeEffect(t *testing.T) {
	for _, tc := range []struct {
		mode         harness.PermissionMode
		wantApproval int32
		wantEffect   int32
	}{
		{harness.PermissionModeOff, 0, 1},
		{harness.PermissionModeDangerous, 0, 1},
		{harness.PermissionModeAll, 1, 0},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			var approvals, effects atomic.Int32
			s, session, _ := runtimeWithDurableInteractionMode(t, toolScript(), &receiptEffectTool{run: func(context.Context) (string, error) {
				effects.Add(1)
				return "read", nil
			}}, false, tc.mode)
			result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "read"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals.Add(1)
				return harness.RejectOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" || approvals.Load() != tc.wantApproval || effects.Load() != tc.wantEffect {
				t.Fatalf("mode=%s result=%+v err=%v approvals=%d effects=%d", tc.mode, result, err, approvals.Load(), effects.Load())
			}
		})
	}
}

func TestRuntimePermissionModeAllGatesNativeTools(t *testing.T) {
	for _, name := range []string{"task", "write_todos"} {
		t.Run(name, func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					arguments := `{"todos":[{"content":"Inspect","activeForm":"Inspecting","status":"in_progress"}]}`
					if name == "task" {
						arguments = `{"subagent_type":"general-purpose","prompt":"inspect","description":"inspect"}`
					}
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "native", Type: "function", Function: schema.FunctionCall{Name: name, Arguments: arguments}}}}}), nil
				}
				return textStream("done"), nil
			}}
			var approvals atomic.Int32
			s, session, _ := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { return "unexpected", nil }}, true, harness.PermissionModeAll)
			result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "use native"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
				if p.ToolName != name {
					t.Errorf("unexpected approval request: %s", p.ToolName)
				}
				approvals.Add(1)
				return harness.RejectOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" || approvals.Load() != 1 {
				t.Fatalf("native %s result=%+v err=%v approvals=%d", name, result, err, approvals.Load())
			}
			fake.mu.Lock()
			calls := fake.calls
			fake.mu.Unlock()
			if calls != 2 {
				t.Fatalf("native %s executed or failed to continue after denial: model calls=%d", name, calls)
			}
		})
	}
}

func TestRuntimePermissionModeAllNestedDelegationResume(t *testing.T) {
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
	var approvals, effects atomic.Int32
	s, session, _ := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) {
		effects.Add(1)
		return "child effect", nil
	}}, true, harness.PermissionModeAll)
	var names []string
	result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals.Add(1)
		names = append(names, p.ToolName)
		return harness.AllowOnce, nil
	})
	if err != nil || result.StopReason != "end_turn" || effects.Load() != 1 || approvals.Load() != 2 || len(names) != 2 || names[0] != "task" || names[1] != "read_workspace" {
		receipts, _ := s.Store.ListToolReceipts(context.Background(), session.ID)
		t.Fatalf("nested all result=%+v err=%v effects=%d approvals=%v receipts=%+v", result, err, effects.Load(), names, receipts)
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
