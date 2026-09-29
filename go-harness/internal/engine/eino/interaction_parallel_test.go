package eino

import (
	"context"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type parallelPermissionFixture struct {
	store    *sqlite.Store
	ledger   *durablebudget.Ledger
	req      harness.RunRequest
	scope    durablebudget.Scope
	engine   *Engine
	model    *scriptedModel
	tool     *recordingTool
	broker   *fixtureInteractionBroker
	staged   StagedExecutionCheckpoint
	hooks    ExecutionHooks
	limits   harness.BudgetLimits
	eventsMu sync.Mutex
	events   []harness.RunEvent
}

func newParallelPermissionFixture(t *testing.T, id string) *parallelPermissionFixture {
	t.Helper()
	f := &parallelPermissionFixture{limits: harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 2, MaxTokens: 100000, MaxOutputTokens: 50}, tool: &recordingTool{}, broker: &fixtureInteractionBroker{decisions: make(map[string]harness.PermissionDecision)}}
	f.store, f.ledger, f.req, f.scope = durableFixture(t, id, f.limits)
	f.req.Session.ConfigVersion = 7
	f.model = &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call != 0 {
			return textStream("both approved"), nil
		}
		// Identical tool and arguments make call identity the only distinction.
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "parallel-call-a", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"same"}`}},
			{ID: "parallel-call-b", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"same"}`}},
		}}}), nil
	}}
	f.engine = newTestEngine(t, Config{ChatModel: f.model, Tools: []tool.BaseTool{f.tool}, Budget: f.limits, BudgetLedger: f.ledger, CheckpointStore: f.store, SessionStore: f.store})
	f.hooks = ExecutionHooks{Broker: f.broker, StageCheckpoint: func(_ context.Context, stage StagedExecutionCheckpoint) error { f.staged = stage; return nil }}
	result, err := f.engine.Run(WithExecutionHooks(durablebudget.WithScope(context.Background(), f.scope), f.hooks), f.req, f.emit, nil)
	if err != nil || result.StopReason != "waiting_input" {
		t.Fatalf("parallel pause: %+v %v", result, err)
	}
	if f.staged.Remove || len(f.staged.Data) == 0 || len(f.staged.Interrupts) != 2 || f.tool.calls.Load() != 0 {
		t.Fatalf("unsafe parallel pause: staged=%+v tool calls=%d", f.staged, f.tool.calls.Load())
	}
	if f.staged.Interrupts[0].IntentID == f.staged.Interrupts[1].IntentID || f.staged.Interrupts[0].NativeInterruptID == f.staged.Interrupts[1].NativeInterruptID {
		t.Fatal("parallel tools shared a permission identity")
	}
	f.eventsMu.Lock()
	starts := map[string]int{}
	for _, event := range f.events {
		if event.Kind == "tool_execute" || event.Kind == "tool_end" {
			f.eventsMu.Unlock()
			t.Fatalf("pending parallel permission produced terminal/effect event: %+v", event)
		}
		if event.Kind == "tool_start" {
			starts[event.ToolCallID]++
		}
	}
	f.eventsMu.Unlock()
	if len(starts) != 2 || starts["parallel-call-a"] != 1 || starts["parallel-call-b"] != 1 {
		t.Fatalf("pending receipt identities: %v", starts)
	}
	f.broker.mu.Lock()
	if f.broker.prepared != 2 || len(f.broker.requests) != 2 {
		f.broker.mu.Unlock()
		t.Fatal("parallel permission preparation was not one per call")
	}
	for _, binding := range f.staged.Interrupts {
		request, ok := f.broker.requests[binding.IntentID]
		if !ok || request.ConfigVersion != 7 || request.ToolName != "read_workspace" || string(request.Arguments) != `{"value":"same"}` {
			f.broker.mu.Unlock()
			t.Fatalf("permission binding lost accepted request: %+v", request)
		}
	}
	f.broker.mu.Unlock()
	if _, exists, err := f.store.Get(context.Background(), f.staged.ID); err != nil || exists {
		t.Fatalf("checkpoint published before host commit: %v %v", exists, err)
	}
	if err = f.store.Set(context.Background(), f.staged.ID, f.staged.Data); err != nil {
		t.Fatal(err)
	}
	if err = f.ledger.EndAttempt(context.Background(), f.scope, durablebudget.OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *parallelPermissionFixture) emit(_ context.Context, event harness.RunEvent) error {
	f.eventsMu.Lock()
	defer f.eventsMu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *parallelPermissionFixture) approveAll(t *testing.T) {
	t.Helper()
	f.scope.Fence = 2
	f.scope.AttemptID = f.req.RunID + "/2"
	if err := f.ledger.BeginAttempt(context.Background(), f.scope); err != nil {
		t.Fatal(err)
	}
	f.hooks.Targets = make(map[string]PermissionResume, 2)
	f.broker.mu.Lock()
	defer f.broker.mu.Unlock()
	for _, binding := range f.staged.Interrupts {
		f.broker.decisions[binding.IntentID] = harness.AllowOnce
		f.hooks.Targets[binding.NativeInterruptID] = PermissionResume{IntentID: binding.IntentID, IntentVersion: binding.IntentVersion, GrantID: "grant/" + binding.IntentID}
	}
}

func (f *parallelPermissionFixture) assertCompleted(t *testing.T, checkpointID string) {
	t.Helper()
	result, err := f.engine.Resume(WithExecutionHooks(durablebudget.WithScope(context.Background(), f.scope), f.hooks), f.req, checkpointID, f.emit, nil)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("parallel resume: %+v %v", result, err)
	}
	f.broker.mu.Lock()
	prepared, resolved := f.broker.prepared, f.broker.resolved
	f.broker.mu.Unlock()
	if f.tool.calls.Load() != 2 || prepared != 2 || resolved != 2 {
		t.Fatalf("parallel resume calls=%d prepared=%d resolved=%d", f.tool.calls.Load(), prepared, resolved)
	}
	if !f.staged.Remove {
		t.Fatal("completion did not stage checkpoint removal")
	}
	snapshot, err := f.ledger.Snapshot(context.Background(), f.scope.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ToolCalls != 2 || snapshot.ModelCalls != 2 || snapshot.HeldTokens != 0 {
		t.Fatalf("parallel permission resume charged again: %+v", snapshot)
	}
	f.eventsMu.Lock()
	defer f.eventsMu.Unlock()
	starts, executes, ends := map[string]int{}, map[string]int{}, map[string]int{}
	for _, event := range f.events {
		switch event.Kind {
		case "tool_start":
			starts[event.ToolCallID]++
		case "tool_execute":
			executes[event.ToolCallID]++
		case "tool_end":
			ends[event.ToolCallID]++
		}
	}
	for _, id := range []string{"parallel-call-a", "parallel-call-b"} {
		if starts[id] != 1 || executes[id] != 1 || ends[id] != 1 {
			t.Fatalf("duplicate/missing receipt lifecycle for %s: start=%d execute=%d end=%d", id, starts[id], executes[id], ends[id])
		}
	}
}

func TestNativeParallelPermissionsPauseApproveResume(t *testing.T) {
	f := newParallelPermissionFixture(t, "parallel-permissions")
	checkpointID := f.staged.ID
	f.approveAll(t)
	f.assertCompleted(t, checkpointID)
}

func TestNativeParallelPermissionsRejectIncompleteExtraAndCrossIntentTargets(t *testing.T) {
	f := newParallelPermissionFixture(t, "parallel-target-validation")
	checkpointID := f.staged.ID
	f.approveAll(t)
	bindings := append([]ExecutionInterruptBinding(nil), f.staged.Interrupts...)
	trusted := f.hooks.Targets
	cases := map[string]func(map[string]PermissionResume){
		"missing": func(targets map[string]PermissionResume) { delete(targets, bindings[0].NativeInterruptID) },
		"extra": func(targets map[string]PermissionResume) {
			targets["unrelated-native-target"] = targets[bindings[0].NativeInterruptID]
		},
		"cross-intent": func(targets map[string]PermissionResume) {
			a, b := bindings[0].NativeInterruptID, bindings[1].NativeInterruptID
			targets[a], targets[b] = targets[b], targets[a]
		},
		"wrong-native-target": func(targets map[string]PermissionResume) {
			old := targets[bindings[0].NativeInterruptID]
			delete(targets, bindings[0].NativeInterruptID)
			targets["unrelated-native-target"] = old
		},
		"stale-intent-version": func(targets map[string]PermissionResume) {
			key := bindings[0].NativeInterruptID
			value := targets[key]
			value.IntentVersion++
			targets[key] = value
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			targets := make(map[string]PermissionResume, len(trusted))
			for key, value := range trusted {
				targets[key] = value
			}
			mutate(targets)
			hooks := f.hooks
			hooks.Targets = targets
			if _, err := f.engine.Resume(WithExecutionHooks(durablebudget.WithScope(context.Background(), f.scope), hooks), f.req, checkpointID, f.emit, nil); err == nil {
				t.Fatal("invalid target set accepted")
			}
			f.broker.mu.Lock()
			resolved, prepared := f.broker.resolved, f.broker.prepared
			f.broker.mu.Unlock()
			f.model.mu.Lock()
			modelCalls := f.model.calls
			f.model.mu.Unlock()
			if resolved != 0 || prepared != 2 || f.tool.calls.Load() != 0 || modelCalls != 1 {
				t.Fatalf("invalid targets reached authorization/effect/model: resolved=%d prepared=%d tools=%d models=%d", resolved, prepared, f.tool.calls.Load(), modelCalls)
			}
		})
	}
	// Invalid target probes must leave the same native checkpoint resumable.
	f.assertCompleted(t, checkpointID)
}

func TestNativeParallelPermissionsResumeAfterSQLiteAndEngineReconstruction(t *testing.T) {
	f := newParallelPermissionFixture(t, "parallel-reconstruction")
	checkpointID := f.staged.ID
	var seq int
	var name, path string
	if err := f.store.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	ledger, err := durablebudget.New(reopened.DB(), durablebudget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.ledger = reopened, ledger
	// A fresh model only receives the native resumed tool results; it must not
	// repeat the first two-call planning response to make this test succeed.
	rebuiltModel := &scriptedModel{stream: func(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		seen := map[string]bool{}
		for _, message := range input {
			if message.Role == schema.Tool {
				seen[message.ToolCallID] = true
			}
		}
		if !seen["parallel-call-a"] || !seen["parallel-call-b"] {
			t.Errorf("rebuilt model lost native tool results: %v", seen)
		}
		return textStream("reconstructed and approved"), nil
	}}
	f.engine = newTestEngine(t, Config{ChatModel: rebuiltModel, Tools: []tool.BaseTool{f.tool}, Budget: f.limits, BudgetLedger: ledger, CheckpointStore: reopened, SessionStore: reopened})
	f.approveAll(t)
	f.assertCompleted(t, checkpointID)
	if rebuiltModel.calls != 1 {
		t.Fatalf("reconstruction made %d provider calls", rebuiltModel.calls)
	}
}
