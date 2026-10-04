package eino

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestConfiguredRoleApprovalDoesNotHoldExecutionSlot(t *testing.T) {
	controller, _ := agents.NewController(nil)
	req := request("approval-slot")
	var roleID string
	var executed atomic.Bool
	underlying := &receiptEffectTool{run: func(ctx context.Context) (string, error) {
		executed.Store(true)
		blocked, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if release, err := controller.Acquire(blocked, req.RunID, "probe"); !errors.Is(err, context.DeadlineExceeded) {
			if release != nil {
				release()
			}
			t.Errorf("actual tool did not retain its slot: %v", err)
		}
		return "read completed", nil
	}}
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return delegateReaderMessage(), nil
		}
		if call == 1 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "approval-read", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
		}
		return textStream("done"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Delegations: controller, Tools: []tool.BaseTool{underlying}, Subagents: &harness.SubagentsConfig{MaxConcurrent: rolePointer(1), CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "reader", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(context.Background(), req, nil, func(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		if p.ToolName != "read_workspace" {
			return harness.AllowOnce, nil
		}
		work := ctx.Value(roleWorkKey{}).(roleWork)
		roleID = work.id
		spec, err := e.ResolveRole("reader", "", nil)
		if err != nil {
			return "", err
		}
		if err = controller.Register(ctx, agents.Invocation{RootID: req.RunID, ID: "probe", Depth: 1, Spec: spec}, 1, 20); err != nil {
			return "", err
		}
		free, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		release, err := controller.Acquire(free, req.RunID, "probe")
		if err != nil {
			return "", err
		}
		release()
		return harness.AllowOnce, nil
	})
	if err != nil || result.StopReason != "end_turn" || !executed.Load() {
		t.Fatalf("result=%+v executed=%v err=%v", result, executed.Load(), err)
	}
	clock, err := controller.Timing(context.Background(), roleID)
	_, hasApproval := clock.Activities["approval"]
	_, hasModel := clock.Activities["model"]
	// Instant fixture callbacks can be shorter than the host clock resolution.
	if err != nil || !hasApproval || !hasModel || clock.Activities["tool"] < 20*time.Millisecond {
		t.Fatalf("timing=%+v err=%v", clock, err)
	}
}

func TestConfiguredRoleTimeSettlementFailureStopsParent(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "time.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	controller, err := agents.NewController(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return delegateReaderMessage(), nil
		}
		if call == 1 {
			if _, err := store.DB().Exec(`CREATE TRIGGER fail_role_settlement BEFORE UPDATE OF elapsed_ns ON harness_agent_invocations BEGIN SELECT RAISE(ABORT,'fixture time settlement failed'); END`); err != nil {
				t.Error(err)
			}
			return textStream("child done"), nil
		}
		t.Error("parent continued before role time settlement was durable")
		return textStream("unexpected"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Delegations: controller, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "reader", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Run(context.Background(), request("settlement-failed"), nil, allowTool)
	var persistence interface{ PersistenceFailure() bool }
	if !errors.As(err, &persistence) || !persistence.PersistenceFailure() {
		t.Fatalf("lost time settlement failure: %v", err)
	}
}

func TestDefaultRoleCatalogAndLegacyReconstruction(t *testing.T) {
	e, err := New(context.Background(), Config{ChatModel: &scriptedModel{}})
	if err != nil {
		t.Fatal(err)
	}
	if !e.HasRoles() || e.config.Delegations == nil {
		t.Fatal("missing config bypassed role admission")
	}
	catalog, err := e.RoleCatalog()
	if err != nil || catalog == nil {
		t.Fatalf("default catalog=%+v err=%v", catalog, err)
	}
	legacy, err := e.WithRoleCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.HasRoles() || legacy.config.Subagents != nil {
		t.Fatal("old checkpoint changed its agent graph")
	}
	if !e.HasRoles() {
		t.Fatal("legacy reconstruction mutated new-run defaults")
	}
}
