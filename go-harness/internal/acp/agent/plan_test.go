package agent_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestACPPlanUpdatesAndHistoryReplay(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for _, plan := range [][]harness.PlanEntry{
			{{Content: "Inspect", Status: "in_progress", Priority: "high"}, {Content: "Report", Status: "pending", Priority: "medium"}},
			{},
		} {
			if err := emit(ctx, harness.RunEvent{Kind: "plan_update", Plan: plan}); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	readPlans := func(responseID int) {
		t.Helper()
		var plans [][]harness.PlanEntry
		for {
			msg := c.read(t)
			if msg.Method == "" {
				if string(msg.ID) != strconv.Itoa(responseID) || msg.Error != nil {
					t.Fatalf("plan response=%+v", msg)
				}
				break
			}
			var wire struct {
				SessionID string `json:"sessionId"`
				Update    struct {
					Kind    string              `json:"sessionUpdate"`
					Entries []harness.PlanEntry `json:"entries"`
				} `json:"update"`
			}
			if msg.Method != "session/update" || json.Unmarshal(msg.Params, &wire) != nil || wire.SessionID != sid {
				t.Fatalf("plan update=%+v", msg)
			}
			if wire.Update.Kind == "plan" {
				if wire.Update.Entries == nil {
					t.Fatalf("plan entries must be an array: %s", msg.Params)
				}
				plans = append(plans, wire.Update.Entries)
			}
		}
		if len(plans) != 2 || len(plans[0]) != 2 || plans[0][0].Content != "Inspect" || plans[0][0].Status != "in_progress" || plans[0][0].Priority != "high" || len(plans[1]) != 0 {
			t.Fatalf("ACP plans=%+v", plans)
		}
	}
	readPlans(c.request(t, "session/prompt", promptParams(sid, "make a plan")))
	c.success(t, c.request(t, "session/close", map[string]any{"sessionId": sid}))
	readPlans(c.request(t, "session/load", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}}))
}
