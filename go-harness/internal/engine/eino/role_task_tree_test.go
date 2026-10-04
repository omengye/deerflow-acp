package eino

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestRoleTaskTreeStopNeverHidesUnconfirmedEffect(t *testing.T) {
	for _, failure := range []error{harness.ErrCommandUncertain, &roleEffectFailure{cause: errors.New("unconfirmed receipt")}, errors.New("storage failure")} {
		joined := fmt.Errorf("role stopped: %w", errors.Join(agents.ErrStopped, context.Canceled, failure))
		remaining := withoutRoleStop(joined)
		if !errors.Is(remaining, failure) || recoverableRoleTimeout(remaining) {
			t.Fatalf("cancel swallowed independent failure: %v", remaining)
		}
	}
	if !recoverableRoleTimeout(withoutRoleStop(errors.Join(agents.ErrStopped, context.Canceled))) {
		t.Fatal("joined safe cancellation not recoverable")
	}
}

func TestRoleTaskTreeChildAdmissionStopBeforeMonitorKeepsParentAlive(t *testing.T) {
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "child-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	c, err := agents.NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var effects, continued atomic.Int32
	var roleID string
	fake := &scriptedModel{stream: func(childCtx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return delegateReaderMessage(), nil
		}
		if call == 1 {
			work := childCtx.Value(roleWorkKey{}).(roleWork)
			roleID = work.id
			if _, err := native.DB().ExecContext(childCtx, `INSERT INTO harness_task_tree_stops VALUES(?,?,?)`, work.rootID, work.id, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return nil, err
			}
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "fenced", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
		}
		continued.Add(1)
		return textStream("parent survives child gate cancellation"), nil
	}}
	e, err := New(ctx, Config{ChatModel: fake, Delegations: c, Tools: []tool.BaseTool{&receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "unexpected", nil }}}, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(ctx, request("child-gate"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || effects.Load() != 0 || continued.Load() != 1 {
		t.Fatalf("result=%+v err=%v effects=%d continued=%d", result, err, effects.Load(), continued.Load())
	}
	var state string
	if err = native.DB().QueryRow(`SELECT state FROM harness_agent_states WHERE invocation_id=?`, roleID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestRoleTaskTreeRootAdmissionStopBeforeMonitorIsExplicitCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var effects, extraCalls atomic.Int32
	var stop func(context.Context) error
	fake := &scriptedModel{stream: func(modelCtx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call > 0 {
			extraCalls.Add(1)
			return textStream("unexpected model call"), nil
		}
		if err := stop(modelCtx); err != nil {
			return nil, err
		}
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "root-fenced", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
	}}
	s, session, native := runtimeWithDurableInteraction(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "unexpected", nil }}, false)
	config := s.Engine.(*Engine).config
	var err error
	config.Delegations, err = agents.NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := durablebudget.New(native.DB(), durablebudget.Config{CheckEffectTx: func(ctx context.Context, tx *sql.Tx, scope durablebudget.Scope) error {
		return CheckTaskTreeEffectTx(ctx, tx, scope.RootBudgetID)
	}})
	if err != nil {
		t.Fatal(err)
	}
	config.BudgetLedger = ledger
	s.Store.BudgetLedger = ledger
	s.Engine, err = New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	stop = func(modelCtx context.Context) error {
		scope, ok := durablebudget.ScopeFromContext(modelCtx)
		if !ok {
			return errors.New("no budget scope")
		}
		_, err := native.DB().ExecContext(modelCtx, `INSERT INTO harness_task_tree_stops VALUES(?,'',?)`, scope.RootBudgetID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}
	result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "stop before next effect"}}, nil, allowTool)
	if err != nil || result.StopReason != "cancelled" || result.Execution == nil || result.Execution.Status != harness.ExecutionCancelled || effects.Load() != 0 || extraCalls.Load() != 0 {
		t.Fatalf("result=%+v execution=%+v err=%v effects=%d extraCalls=%d", result, result.Execution, err, effects.Load(), extraCalls.Load())
	}
	receipts, err := s.Store.ListToolReceipts(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.State != harness.ReceiptNotExecuted {
			t.Fatalf("root cancellation receipt=%+v", receipt)
		}
	}
}

func TestRoleTaskTreePausedCancelReconstructsWithoutApprovalOrEffects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var effects, approvals atomic.Int32
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return delegateReaderMessage(), nil
		case 1:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "paused-child", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
		default:
			found := false
			for _, m := range input {
				if m.Role == schema.Tool && strings.Contains(m.Content, "subagent execution failed") {
					found = true
				}
			}
			if !found {
				t.Error("parent did not receive stopped child result")
			}
			return textStream("parent continued after explicit subtree stop"), nil
		}
	}}
	s, session, native := runtimeWithDurableInteraction(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "unexpected effect", nil }}, true)
	config := s.Engine.(*Engine).config
	var err error
	config.Delegations, err = agents.NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	config.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "read", Tools: []string{"read_workspace"}}}}
	s.Engine, err = New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.PermissionCancelled, nil
	})
	if err != nil || paused.Execution == nil || paused.Execution.Status != harness.ExecutionWaitingInput {
		t.Fatalf("pause=%+v execution=%+v err=%v", paused, paused.Execution, err)
	}
	actor := harness.TaskActor{SessionID: session.ID, OwnerID: "owner"}
	page, err := s.TaskTree(ctx, actor, harness.TaskTreeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var role harness.TaskTreeNode
	for _, n := range page.Nodes {
		if n.Mode == "foreground" {
			role = n
		}
	}
	if role.ID == "" || role.Status != "waiting_input" || role.Execution == nil || role.Execution.RunID != paused.Execution.RunID || !role.Execution.Resumable {
		t.Fatalf("paused role reference=%+v", role)
	}
	stopped, err := s.CancelTaskTree(ctx, actor, harness.TaskTreeCancelRequest{RootID: role.RootID, NodeID: role.ID, ExpectedVersion: role.Version})
	if err != nil || stopped.Status != "cancelling" || stopped.Execution == nil || stopped.Execution.Version <= paused.Execution.Version {
		t.Fatalf("cancel=%+v %v", stopped, err)
	}
	if _, err = s.ResumeExecution(ctx, "owner", session.ID, harness.ResumeExecutionRequest{RunID: paused.Execution.RunID, ExpectedVersion: paused.Execution.Version}, nil, allowTool); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("old resume accepted=%v", err)
	}
	if err = s.Disconnect(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(ctx, "new-owner", session.ID, session.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	config.Delegations, err = agents.NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	s.Engine, err = New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.Execution(ctx, "new-owner", session.ID, paused.Execution.RunID)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ResumeExecution(ctx, "new-owner", session.ID, harness.ResumeExecutionRequest{RunID: current.RunID, ExpectedVersion: current.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals.Add(1)
		return harness.AllowOnce, nil
	})
	if err != nil || resumed.Execution == nil || resumed.Execution.Status != harness.ExecutionCompleted || effects.Load() != 0 || approvals.Load() != 0 {
		t.Fatalf("resume=%+v execution=%+v err=%v effects=%d approvals=%d", resumed, resumed.Execution, err, effects.Load(), approvals.Load())
	}
	actor.OwnerID = "new-owner"
	final, err := s.TaskTreeNode(ctx, actor, role.RootID, role.ID)
	if err != nil || final.Status != "cancelled" || final.Execution != nil {
		t.Fatalf("final=%+v %v", final, err)
	}
	receipts, err := s.Store.ListToolReceipts(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].State != harness.ReceiptNotExecuted {
		t.Fatalf("cancelled role receipts=%+v", receipts)
	}
	var grants, pausedOps int
	if err = native.DB().QueryRow(`SELECT (SELECT count(*) FROM harness_execution_grants),(SELECT count(*) FROM budget_reservations WHERE kind='tool' AND state='paused')`).Scan(&grants, &pausedOps); err != nil || grants != 0 || pausedOps != 0 {
		t.Fatalf("grants=%d paused reservations=%d error=%v", grants, pausedOps, err)
	}
}

func TestRoleTaskTreeRunningSubtreeStopsAfterIOJoinsAndParentContinues(t *testing.T) {
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	controller, err := agents.NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var joined, continued atomic.Bool
	var roleID string
	fake := &scriptedModel{stream: func(childCtx context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return delegateReaderMessage(), nil
		}
		if call == 1 {
			work, ok := childCtx.Value(roleWorkKey{}).(roleWork)
			if !ok {
				return nil, errors.New("missing role context")
			}
			roleID = work.id
			if _, err := native.DB().ExecContext(childCtx, `INSERT INTO harness_task_tree_stops VALUES(?,?,?)`, work.rootID, work.id, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return nil, err
			}
			<-childCtx.Done()
			time.Sleep(25 * time.Millisecond)
			joined.Store(true)
			return nil, childCtx.Err()
		}
		continued.Store(true)
		if !joined.Load() {
			t.Error("parent continued before provider cleanup")
		}
		found := false
		for _, m := range input {
			if m.Role == schema.Tool && strings.Contains(m.Content, "subagent execution failed") {
				found = true
			}
		}
		if !found {
			t.Error("parent did not receive safe cancellation result")
		}
		return textStream("parent continued safely"), nil
	}}
	e, err := New(ctx, Config{ChatModel: fake, Delegations: controller, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "reader", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(ctx, request("scope-cancel"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || !joined.Load() || !continued.Load() {
		t.Fatalf("result=%+v joined=%v continued=%v error=%v", result, joined.Load(), continued.Load(), err)
	}
	var state string
	if err = native.DB().QueryRow(`SELECT state FROM harness_agent_states WHERE invocation_id=?`, roleID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("journal=%s %v", state, err)
	}
	var active int
	if err = native.DB().QueryRow(`SELECT count(*) FROM harness_agent_attempts WHERE state='active'`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("unjoined attempts=%d %v", active, err)
	}
}
