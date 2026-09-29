package eino

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type fixturePolicyBroker struct {
	*fixtureInteractionBroker
	policy func(context.Context, harness.PermissionRequest, PermissionIntent) (harness.PermissionDecision, bool, error)
}

func (b *fixturePolicyBroker) ResolvePolicyPermission(ctx context.Context, req harness.PermissionRequest, intent PermissionIntent) (harness.PermissionDecision, bool, error) {
	return b.policy(ctx, req, intent)
}

var _ interaction.PolicyPermissionBroker = (*fixturePolicyBroker)(nil)

func TestNativePermissionInheritedPolicyKeepsAdmissionAndEventGates(t *testing.T) {
	policyErr := errors.New("policy grant transaction failed")
	for _, test := range []struct {
		name       string
		decision   harness.PermissionDecision
		handled    bool
		absent     bool
		policyErr  error
		wantErr    error
		wantEffect bool
		wantWait   bool
	}{
		{name: "allow_once", decision: harness.AllowOnce, handled: true, wantEffect: true},
		{name: "allow_always", decision: harness.AllowAlways, handled: true, wantEffect: true},
		{name: "reject_once", decision: harness.RejectOnce, handled: true},
		{name: "reject_always", decision: harness.RejectAlways, handled: true},
		{name: "unconfigured", wantWait: true},
		{name: "foreground_without_interface", absent: true, wantWait: true},
		{name: "policy_error", handled: true, policyErr: policyErr, wantErr: policyErr},
		{name: "unhandled_error", policyErr: policyErr, wantErr: policyErr},
		{name: "invalid_decision", decision: harness.PermissionCancelled, handled: true, wantErr: harness.ErrInvalidInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 1, MaxTokens: 100000, MaxOutputTokens: 50}
			_, ledger, req, scope := durableFixture(t, "policy-"+test.name, limits)
			req.Session.ConfigVersion = 7
			underlying := &recordingTool{}
			e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger})
			base := &fixtureInteractionBroker{}
			policyCalls := 0
			var broker InteractionBroker = &fixturePolicyBroker{fixtureInteractionBroker: base, policy: func(ctx context.Context, p harness.PermissionRequest, intent PermissionIntent) (harness.PermissionDecision, bool, error) {
				policyCalls++
				if p.RunID != req.RunID || p.SessionID != req.Session.ID || p.ConfigVersion != 7 || intent.ID != p.ID || intent.Version != 1 || base.prepared != 1 {
					return "", false, errors.New("policy invoked before bound intent")
				}
				snapshot, err := ledger.Snapshot(ctx, scope.RootBudgetID)
				if err != nil {
					return "", false, err
				}
				if snapshot.ToolCalls != 1 {
					return "", false, errors.New("policy invoked before tool budget admission")
				}
				return test.decision, test.handled, test.policyErr
			}}
			if test.absent {
				broker = base
			}
			var staged StagedExecutionCheckpoint
			hooks := ExecutionHooks{Broker: broker, StageCheckpoint: func(_ context.Context, checkpoint StagedExecutionCheckpoint) error { staged = checkpoint; return nil }}
			ctx := WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
			events := map[string]int{}
			result, err := e.Run(ctx, req, func(_ context.Context, event harness.RunEvent) error { events[event.Kind]++; return nil }, nil)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v result=%+v", err, test.wantErr, result)
			}
			if test.wantErr == nil {
				wantReason := "end_turn"
				if test.wantWait {
					wantReason = "waiting_input"
				}
				if result.StopReason != wantReason {
					t.Fatalf("reason=%s want=%s", result.StopReason, wantReason)
				}
			}
			wantEffects := int32(0)
			if test.wantEffect {
				wantEffects = 1
			}
			if underlying.calls.Load() != wantEffects || events["tool_execute"] != int(wantEffects) || events["tool_start"] != 1 || base.prepared != 1 || base.resolved != 0 {
				t.Fatalf("effects=%d events=%v prepared=%d resolved=%d", underlying.calls.Load(), events, base.prepared, base.resolved)
			}
			wantCalls := 1
			if test.absent {
				wantCalls = 0
			}
			if policyCalls != wantCalls {
				t.Fatalf("policy calls=%d want=%d", policyCalls, wantCalls)
			}
			if test.wantWait {
				if events["tool_end"] != 0 || len(staged.Interrupts) != 1 || len(staged.Data) == 0 {
					t.Fatalf("unconfigured policy lost native interrupt: events=%v staged=%+v", events, staged)
				}
			} else if events["tool_end"] != 1 || len(staged.Interrupts) != 0 {
				t.Fatalf("policy bypassed terminal gate: events=%v staged=%+v", events, staged)
			}
			snapshot, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
			if err != nil || snapshot.ToolCalls != 1 || snapshot.HeldTokens != 0 {
				t.Fatalf("policy changed budget accounting: %+v %v", snapshot, err)
			}
		})
	}
}
