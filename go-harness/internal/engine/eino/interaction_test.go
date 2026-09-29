package eino

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

type fixtureInteractionBroker struct {
	mu                 sync.Mutex
	requests           map[string]harness.PermissionRequest
	decisions          map[string]harness.PermissionDecision
	prepared, resolved int
}

func (b *fixtureInteractionBroker) PreparePermission(_ context.Context, req harness.PermissionRequest) (PermissionIntent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.requests == nil {
		b.requests = make(map[string]harness.PermissionRequest)
	}
	b.requests[req.ID] = req
	b.prepared++
	return PermissionIntent{ID: req.ID, Version: 1}, nil
}
func (b *fixtureInteractionBroker) ResolvePermission(_ context.Context, req harness.PermissionRequest, intent PermissionIntent, grant string) (harness.PermissionDecision, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	stored, ok := b.requests[intent.ID]
	if !ok || permissionHash(stored) != permissionHash(req) || intent.Version != 1 || grant != "grant/"+intent.ID {
		return "", harness.ErrPermissionDenied
	}
	b.resolved++
	return b.decisions[intent.ID], nil
}

func TestNativePermissionPauseResumeUsesOneToolAdmission(t *testing.T) {
	for _, decision := range []harness.PermissionDecision{harness.AllowOnce, harness.RejectOnce} {
		t.Run(string(decision), func(t *testing.T) {
			limits := harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 1, MaxTokens: 100000, MaxOutputTokens: 50}
			_, ledger, req, scope := durableFixture(t, "permission-"+string(decision), limits)
			req.Session.ConfigVersion = 7
			underlying := &recordingTool{}
			fake := toolScript()
			e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger})
			broker := &fixtureInteractionBroker{decisions: make(map[string]harness.PermissionDecision)}
			var staged StagedExecutionCheckpoint
			hooks := ExecutionHooks{Broker: broker, StageCheckpoint: func(_ context.Context, s StagedExecutionCheckpoint) error { staged = s; return nil }}
			ctx := WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
			var events []harness.RunEvent
			result, err := e.Run(ctx, req, func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, nil)
			if err != nil || result.StopReason != "waiting_input" {
				t.Fatalf("pause: %+v %v", result, err)
			}
			if staged.Remove || len(staged.Data) == 0 || len(staged.Interrupts) != 1 || underlying.calls.Load() != 0 || fake.calls != 1 {
				t.Fatalf("unsafe pause: staged=%+v calls=%d models=%d", staged, underlying.calls.Load(), fake.calls)
			}
			for _, event := range events {
				if event.Kind == "tool_execute" || event.Kind == "tool_end" {
					t.Fatalf("pending permission acquired terminal receipt: %+v", event)
				}
			}
			binding := staged.Interrupts[0]
			if broker.requests[binding.IntentID].ConfigVersion != 7 {
				t.Fatal("permission config version lost")
			}
			if _, found, _ := e.config.CheckpointStore.Get(context.Background(), staged.ID); found {
				t.Fatal("engine published runtime-owned checkpoint before commit")
			}
			if err = e.config.CheckpointStore.Set(context.Background(), staged.ID, staged.Data); err != nil {
				t.Fatal(err)
			}
			if err = ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeWaitingInput); err != nil {
				t.Fatal(err)
			}
			scope.Fence = 2
			scope.AttemptID = req.RunID + "/2"
			if err = ledger.BeginAttempt(context.Background(), scope); err != nil {
				t.Fatal(err)
			}
			broker.decisions[binding.IntentID] = decision
			hooks.Targets = map[string]PermissionResume{binding.NativeInterruptID: {IntentID: binding.IntentID, IntentVersion: binding.IntentVersion, GrantID: "grant/" + binding.IntentID}}
			ctx = WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
			result, err = e.Resume(ctx, req, staged.ID, nil, nil)
			if err != nil || result.StopReason != "end_turn" {
				t.Fatalf("resume: %+v %v", result, err)
			}
			want := int32(0)
			if decision == harness.AllowOnce {
				want = 1
			}
			if underlying.calls.Load() != want || broker.prepared != 1 || broker.resolved != 1 {
				t.Fatalf("permission lifecycle: calls=%d prepared=%d resolved=%d", underlying.calls.Load(), broker.prepared, broker.resolved)
			}
			snapshot, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.ToolCalls != 1 || snapshot.ModelCalls != 2 || snapshot.HeldTokens != 0 {
				t.Fatalf("resume charged a second logical tool: %+v", snapshot)
			}
			if !staged.Remove {
				t.Fatal("completed native checkpoint removal was not staged")
			}
		})
	}
}

func TestPermissionResumeRequiresOriginalInputAndTrustedTarget(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 1}
	_, ledger, req, scope := durableFixture(t, "permission-binding", limits)
	underlying := &recordingTool{}
	e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger})
	broker := &fixtureInteractionBroker{decisions: make(map[string]harness.PermissionDecision)}
	var staged StagedExecutionCheckpoint
	hooks := ExecutionHooks{Broker: broker, StageCheckpoint: func(_ context.Context, s StagedExecutionCheckpoint) error { staged = s; return nil }}
	if _, err := e.Run(WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks), req, nil, nil); err != nil {
		t.Fatal(err)
	}
	key := staged.ID
	if err := e.config.CheckpointStore.Set(context.Background(), key, staged.Data); err != nil {
		t.Fatal(err)
	}
	if err := ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	scope.Fence = 2
	scope.AttemptID = req.RunID + "/2"
	if err := ledger.BeginAttempt(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
	if _, err := e.Resume(ctx, req, key, nil, nil); err == nil {
		t.Fatal("missing targets accepted")
	}
	binding := staged.Interrupts[0]
	hooks.Targets = map[string]PermissionResume{binding.NativeInterruptID: {IntentID: binding.IntentID, IntentVersion: binding.IntentVersion, GrantID: "grant/" + binding.IntentID}}
	ctx = WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
	bad := req
	bad.InputID = "replacement-input"
	if _, err := e.Resume(ctx, bad, key, nil, nil); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("changed input identity: %v", err)
	}
	if underlying.calls.Load() != 0 || broker.resolved != 0 {
		t.Fatal("invalid resume reached authorization or effect")
	}
}
