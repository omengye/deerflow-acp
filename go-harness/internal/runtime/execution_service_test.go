package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type durableRuntimeEngine struct {
	t        *testing.T
	accepted harness.RunRequest
	onEffect func()
	resumes  atomic.Int32
}

func (*durableRuntimeEngine) DurableExecutions() bool { return true }
func durablePermission(req harness.RunRequest) harness.PermissionRequest {
	return harness.PermissionRequest{ID: req.RunID + "/call", SessionID: req.Session.ID, RunID: req.RunID, ConfigVersion: req.Session.ConfigVersion, ToolCallID: "call", ToolName: "write_file", Arguments: json.RawMessage("{\n  \"path\": \"test.txt\", \"nested\": { \"message\": \"hello\" }\n}")}
}
func (e *durableRuntimeEngine) Run(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
	e.accepted = req
	hooks := interaction.FromContext(ctx)
	if hooks == nil || hooks.Broker == nil {
		e.t.Fatal("trusted execution hooks missing")
	}
	p := durablePermission(req)
	if err := emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "pending", Arguments: p.Arguments}); err != nil {
		return harness.RunResult{}, err
	}
	intent, err := hooks.Broker.PreparePermission(ctx, p)
	if err != nil {
		return harness.RunResult{}, err
	}
	err = hooks.StageCheckpoint(ctx, interaction.StagedExecutionCheckpoint{ID: "harness/turn/v1/" + req.RunID, Data: []byte("native opaque checkpoint"), Interrupts: []interaction.ExecutionInterruptBinding{{IntentID: intent.ID, IntentVersion: intent.Version, NativeInterruptID: "native/call"}}})
	return harness.RunResult{StopReason: "waiting_input"}, err
}
func (e *durableRuntimeEngine) Resume(ctx context.Context, req harness.RunRequest, key string, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
	e.resumes.Add(1)
	if key != "harness/turn/v1/"+req.RunID || (e.accepted.RunID != "" && !reflect.DeepEqual(e.accepted, req)) {
		e.t.Fatal("resume changed accepted request")
	}
	scope, ok := budget.ScopeFromContext(ctx)
	if !ok || scope.AttemptID == req.RunID || scope.MemberID != req.RunID || scope.Fence < 2 {
		e.t.Fatal("resume reused foreground attempt")
	}
	hooks := interaction.FromContext(ctx)
	target, ok := hooks.Targets["native/call"]
	if !ok {
		e.t.Fatal("native target missing")
	}
	p := durablePermission(req)
	decision, err := hooks.Broker.ResolvePermission(ctx, p, interaction.PermissionIntent{ID: target.IntentID, Version: target.IntentVersion}, target.GrantID)
	if err != nil {
		return harness.RunResult{}, err
	}
	if decision == harness.AllowOnce || decision == harness.AllowAlways {
		if err = emit(ctx, harness.RunEvent{Kind: "tool_execute", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "in_progress"}); err != nil {
			return harness.RunResult{}, err
		}
		if e.onEffect != nil {
			e.onEffect()
		}
		err = emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "completed"})
	} else {
		err = emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "failed", Content: []harness.Content{{Type: "text", Text: "permission denied"}}})
	}
	if err == nil {
		err = hooks.StageCheckpoint(ctx, interaction.StagedExecutionCheckpoint{ID: key, Remove: true})
	}
	return harness.RunResult{StopReason: "end_turn"}, err
}

func TestDurableRuntimeAutoResumeAndAtomicGrantDispatch(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "allow", false: "deny"}[allow], func(t *testing.T) {
			s, native, _, x := budgetService(t, harness.BudgetLimits{MaxToolCalls: 1}, budget.Config{})
			engine := &durableRuntimeEngine{t: t}
			s.Engine = engine
			effects := 0
			engine.onEffect = func() {
				effects++
				var grant, receipt string
				if err := native.DB().QueryRow(`SELECT g.state,r.state FROM harness_execution_grants g JOIN harness_execution_intents i ON i.id=g.intent_id JOIN harness_tool_receipts r ON r.run_id=i.run_id AND r.tool_call_id=i.tool_call_id`).Scan(&grant, &receipt); err != nil || grant != "consumed" || receipt != "started" {
					t.Fatal("effect escaped atomic grant/receipt commit", grant, receipt, err)
				}
			}
			approvals := 0
			result, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "original prompt"}}, nil, func(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				var active int
				if err := native.DB().QueryRow(`SELECT count(*) FROM budget_attempts WHERE state='active'`).Scan(&active); err != nil || active != 0 {
					t.Fatal("human wait charged active budget", active, err)
				}
				state, err := s.Execution(ctx, "owner", x.ID, p.RunID)
				if err != nil || state.Status != harness.ExecutionWaitingInput || !state.Resumable {
					t.Fatal("approval preceded checkpoint commit", state, err)
				}
				if !strings.Contains(string(p.Arguments), "\n") || !strings.Contains(string(p.Arguments), "{ \"message\"") {
					t.Fatal("broker compacted raw arguments")
				}
				if allow {
					return harness.AllowOnce, nil
				}
				return harness.RejectOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || result.Execution.Attempt != 2 || engine.resumes.Load() != 1 || approvals != 1 {
				t.Fatalf("result=%+v state=%+v err=%v", result, result.Execution, err)
			}
			latest, err := s.Execution(context.Background(), "owner", x.ID, "")
			if err != nil || latest.RunID != result.Execution.RunID {
				t.Fatal("latest execution not discoverable", latest, err)
			}
			if effects != map[bool]int{true: 1, false: 0}[allow] {
				t.Fatal("unexpected tool effects", effects)
			}
			var consumed, inputs, starts int
			if err = native.DB().QueryRow(`SELECT count(*) FROM harness_execution_grants WHERE state='consumed'`).Scan(&consumed); err != nil || consumed != 1 {
				t.Fatal("grant not consumed once", consumed, err)
			}
			if err = native.DB().QueryRow(`SELECT count(*) FROM harness_inputs`).Scan(&inputs); err != nil || inputs != 1 {
				t.Fatal("resume inserted another input")
			}
			if err = native.DB().QueryRow(`SELECT count(*) FROM harness_events WHERE json_extract(CAST(event AS TEXT),'$.kind')='tool_start'`).Scan(&starts); err != nil || starts != 1 {
				t.Fatal("resume duplicated tool_start")
			}
		})
	}
}

func TestDurableRuntimeGrantAndReceiptFailurePreventsEffects(t *testing.T) {
	s, native, _, x := budgetService(t, harness.BudgetLimits{MaxToolCalls: 1}, budget.Config{})
	effects := 0
	s.Engine = &durableRuntimeEngine{t: t, onEffect: func() { effects++ }}
	if _, err := native.DB().Exec(`CREATE TRIGGER fail_dispatch BEFORE UPDATE ON harness_tool_receipts WHEN NEW.state='started' BEGIN SELECT RAISE(ABORT,'fixture dispatch'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err == nil || effects != 0 {
		t.Fatal("effect escaped failed dispatch", effects, err)
	}
	var consumed int
	if err = native.DB().QueryRow(`SELECT count(*) FROM harness_execution_grants WHERE state='consumed'`).Scan(&consumed); err != nil || consumed != 0 {
		t.Fatal("failed receipt consumed grant")
	}
}

func TestDurableRuntimeDisconnectAndExplicitCancelDiffer(t *testing.T) {
	for _, disconnect := range []bool{true, false} {
		t.Run(map[bool]string{true: "disconnect", false: "explicit_cancel"}[disconnect], func(t *testing.T) {
			s, _, _, x := budgetService(t, harness.BudgetLimits{}, budget.Config{})
			engine := &durableRuntimeEngine{t: t}
			s.Engine = engine
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if disconnect {
				ctx = WithTransportCancellation(ctx)
			}
			result, err := s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				cancel()
				return harness.PermissionCancelled, context.Canceled
			})
			want := harness.ExecutionCancelled
			if disconnect {
				want = harness.ExecutionWaitingInput
			}
			if err != nil || result.Execution == nil || result.Execution.Status != want || engine.resumes.Load() != 0 {
				t.Fatalf("result=%+v state=%+v err=%v", result, result.Execution, err)
			}
			if disconnect {
				if _, err = s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "replacement"}}, nil, nil); !errors.Is(err, harness.ErrExecutionWaitingInput) {
					t.Fatal("new prompt bypassed wait", err)
				}
				if err = s.Disconnect(context.Background(), "owner"); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Load(context.Background(), "next-owner", x.ID, x.CWD, false, nil); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Execution(context.Background(), "owner", x.ID, ""); !errors.Is(err, harness.ErrNotAttached) {
					t.Fatal("retired owner read execution", err)
				}
				var approvals int
				continued, err := s.ResumeExecution(context.Background(), "next-owner", x.ID, harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
					approvals++
					return harness.AllowOnce, nil
				})
				if err != nil || continued.Execution == nil || continued.Execution.Status != harness.ExecutionCompleted || approvals != 1 {
					t.Fatalf("continue=%+v state=%+v approvals=%d err=%v", continued, continued.Execution, approvals, err)
				}
			}
		})
	}
}

func TestDurableRuntimeLatestExecutionSelectsNewestRun(t *testing.T) {
	s, _, _, x := budgetService(t, harness.BudgetLimits{}, budget.Config{})
	s.Engine = &durableRuntimeEngine{t: t}
	first, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "first"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "second"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.Execution(context.Background(), "owner", x.ID, "")
	if err != nil || latest.RunID != second.Execution.RunID || latest.RunID == first.Execution.RunID {
		t.Fatal("latest execution selected older run", latest, err)
	}
	old, err := s.Execution(context.Background(), "owner", x.ID, first.Execution.RunID)
	if err != nil || old.RunID != first.Execution.RunID {
		t.Fatal("exact execution lookup changed", old, err)
	}
}

func TestDurableRuntimeExplicitCancelWinsTransportMarker(t *testing.T) {
	s, _, _, x := budgetService(t, harness.BudgetLimits{}, budget.Config{})
	s.Engine = &durableRuntimeEngine{t: t}
	ctx := WithTransportCancellation(context.Background())
	result, err := s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, func(ctx context.Context, _ harness.PermissionRequest) (harness.PermissionDecision, error) {
		if err := s.Coordinator.Cancel(x.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		return harness.PermissionCancelled, ctx.Err()
	})
	if err != nil || result.StopReason != "cancelled" || result.Execution == nil || result.Execution.Status != harness.ExecutionCancelled {
		t.Fatalf("explicit ACP cancel became disconnect: result=%+v state=%+v err=%v", result, result.Execution, err)
	}
}

func TestDurableRuntimeStartupChecksInterruptedBudgetBeforeResume(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "claimed_only", true: "model_reserved"}[inFlight], func(t *testing.T) {
			f := newExecutionFixture(t)
			f.s.BudgetLedger = f.ledger
			f.pending(t)
			f.suspend(t)
			lease := f.resume(t, "interrupted-owner")
			f.grant(t, lease)
			if inFlight {
				if _, err := f.ledger.ReserveModel(context.Background(), lease.Scope, budget.ModelRequest{OperationID: NewID(), Digest: "interrupted", InputTokens: 10, MaxOutputTokens: 10}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.ReconcileInterrupted(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, err := f.s.Execution(context.Background(), f.req.Session.ID, f.req.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if inFlight {
				if state.Status != harness.ExecutionNeedsReconciliation || state.Resumable {
					t.Fatal("unresolved model reservation resumed", state)
				}
			} else {
				if state.Status != harness.ExecutionWaitingInput || !state.Resumable {
					t.Fatal("pure claim did not recover wait", state)
				}
				next := f.resume(t, "fresh-owner")
				if next.Scope.Fence != 3 {
					t.Fatal("startup reused attempt fence")
				}
			}
		})
	}
}

func TestDurableRuntimeEventFailureStopsBeforeEffect(t *testing.T) {
	for _, eventKind := range []string{"tool_start", "tool_execute"} {
		t.Run(eventKind, func(t *testing.T) {
			s, _, _, x := budgetService(t, harness.BudgetLimits{}, budget.Config{})
			effects, approvals := 0, 0
			engine := &durableRuntimeEngine{t: t, onEffect: func() { effects++ }}
			s.Engine = engine
			broken := errors.New("event handler refused execution")
			result, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "write"}}, func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == eventKind {
					return broken
				}
				return nil
			}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				return harness.AllowOnce, nil
			})
			if !errors.Is(err, broken) || effects != 0 || result.Execution == nil || result.Execution.Resumable {
				t.Fatalf("effect escaped event failure: effects=%d result=%+v state=%+v err=%v", effects, result, result.Execution, err)
			}
			if eventKind == "tool_start" && (approvals != 0 || engine.resumes.Load() != 0) {
				t.Fatal("failed tool_start continued into permission/resume", approvals, engine.resumes.Load())
			}
		})
	}
}

func TestDurableRuntimePermissionFailurePreservesCommittedWaitingCheckpoint(t *testing.T) {
	s, native, _, x := budgetService(t, harness.BudgetLimits{}, budget.Config{})
	engine := &durableRuntimeEngine{t: t}
	s.Engine = engine
	broken := errors.New("permission channel lost")
	var committed harness.ExecutionState
	result, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, func(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		var err error
		committed, err = s.Execution(ctx, "owner", x.ID, p.RunID)
		if err != nil || committed.Status != harness.ExecutionWaitingInput || !committed.Resumable {
			t.Fatal("permission requested before waiting checkpoint committed", committed, err)
		}
		return "", broken
	})
	if !errors.Is(err, broken) || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput || !result.Execution.Resumable || result.Execution.Version != committed.Version || engine.resumes.Load() != 0 {
		t.Fatalf("result=%+v state=%+v err=%v", result, result.Execution, err)
	}
	var checkpoints, grants int
	if err = native.DB().QueryRow(`SELECT count(*) FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+committed.RunID).Scan(&checkpoints); err != nil || checkpoints != 1 {
		t.Fatal("permission failure removed committed checkpoint", checkpoints, err)
	}
	if err = native.DB().QueryRow(`SELECT count(*) FROM harness_execution_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("permission failure created a grant", grants, err)
	}
	result, err = s.ResumeExecution(context.Background(), "owner", x.ID, harness.ResumeExecutionRequest{RunID: committed.RunID, ExpectedVersion: committed.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || engine.resumes.Load() != 1 {
		t.Fatalf("committed checkpoint did not resume: result=%+v state=%+v err=%v", result, result.Execution, err)
	}
}

func TestDurableRuntimeReopenPreservesPendingReceipt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	open := func() (*Service, *sqlite.Store) {
		native, err := sqlite.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(ctx, native.DB())
		if err != nil {
			t.Fatal(err)
		}
		ledger, err := budget.New(native.DB(), budget.Config{})
		if err != nil {
			t.Fatal(err)
		}
		store.BudgetLedger = ledger
		if err = store.ReconcileInterrupted(ctx); err != nil {
			t.Fatal(err)
		}
		return NewService(store, &durableRuntimeEngine{t: t}, "test"), native
	}
	s, native := open()
	x, err := s.NewSession(ctx, "one", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Run(ctx, "one", x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.PermissionCancelled, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
		t.Fatal("waiting", result, err)
	}
	if err = s.Disconnect(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err = native.Close(); err != nil {
		t.Fatal(err)
	}
	s, native = open()
	defer native.Close()
	if _, err = s.Load(ctx, "two", x.ID, x.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	state, err := s.Execution(ctx, "two", x.ID, result.Execution.RunID)
	if err != nil || !state.Resumable || len(state.WaitingInputs) != 1 {
		t.Fatalf("reopen state=%+v err=%v", state, err)
	}
	receipts, err := s.ListToolReceipts(ctx, "two", x.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptPending {
		t.Fatal("startup settled waiting permission", receipts, err)
	}
	result, err = s.ResumeExecution(ctx, "two", x.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("resumed=%+v state=%+v err=%v", result, result.Execution, err)
	}
}
