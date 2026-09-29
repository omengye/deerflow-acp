package deerflow

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKDurableBudgetActualToolAndRestart(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"budget-write\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"budget.txt\\\",\\\"content\\\":\\\"persisted\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Next turn\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":30,\"total_tokens\":80}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	limits := harness.BudgetLimits{MaxModelCalls: 1, MaxToolCalls: 3, MaxTokens: 10000, MaxOutputTokens: 256}
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true, Budget: &limits}
	ctx := context.Background()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	workspace := t.TempDir()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	var runID string
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "write budget.txt"}}, func(_ context.Context, event harness.RunEvent) error { runID = event.RunID; return nil }, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.StopReason != "max_turn_requests" || result.Limit != "model_calls" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "budget.txt"))
	if err != nil || string(data) != "persisted" {
		t.Fatalf("tool output=%q err=%v", data, err)
	}
	before, err := c.budgets.Snapshot(ctx, runID)
	if err != nil || before.ModelCalls != 1 || before.ToolCalls != 1 || before.SpentTokens != 80 || before.HeldTokens != 0 {
		t.Fatalf("budget=%+v err=%v", before, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("budget allowed additional provider calls=%d", calls.Load())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	after, err := c.budgets.Snapshot(ctx, runID)
	if err != nil || after.ModelCalls != before.ModelCalls || after.ToolCalls != before.ToolCalls || after.SpentTokens != before.SpentTokens || after.HeldTokens != 0 || after.BlockedReason != before.BlockedReason {
		t.Fatalf("reopened budget=%+v err=%v", after, err)
	}
	if _, err := c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	var nextRunID string
	result, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "next question"}}, func(_ context.Context, event harness.RunEvent) error { nextRunID = event.RunID; return nil }, nil)
	if err != nil || result.StopReason != "end_turn" || nextRunID == runID || calls.Load() != 2 {
		t.Fatalf("next=%+v id=%s err=%v calls=%d", result, nextRunID, err, calls.Load())
	}
	next, err := c.budgets.Snapshot(ctx, nextRunID)
	if err != nil || next.ModelCalls != 1 || next.ToolCalls != 0 || next.SpentTokens != 80 || next.HeldTokens != 0 {
		t.Fatalf("new prompt budget=%+v err=%v", next, err)
	}
}
