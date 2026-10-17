package deerflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestScheduleManagementRunsThroughNativeServiceAndPersists(t *testing.T) {
	ctx := context.Background()
	calls := 0
	cfg := Config{DataDir: t.TempDir(), Engine: engineFunc(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls++
		if r.Session.ApprovalMode != harness.ApprovalReadOnly {
			t.Error("default approval escalated")
		}
		if r.RootBudgetID == "" {
			t.Error("unmetered dispatch")
		}
		return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "verified result"})
	})}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"operation": "schedule.create", "spec": automation.Spec{Name: "test", Workspace: t.TempDir(), Prompt: "Inspect workspace", Kind: "once", At: time.Now().Add(time.Hour)}}
	raw, _ := json.Marshal(request)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	value, err := c.ManageLocal(ctx, localhost.ManagementRequest{Operation: "schedule.create", Fields: fields})
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	created := value.(automation.Schedule)
	if err = c.scheduler.Tick(ctx, time.Now().Add(2*time.Hour)); err != nil {
		c.Close()
		t.Fatal(err)
	}
	h, err := c.scheduler.History(ctx, created.Spec.ID)
	if err != nil || len(h) != 1 || h[0].Outcome.Status != "completed" || h[0].Outcome.Text != "verified result" || h[0].Outcome.SessionID == "" {
		c.Close()
		t.Fatal(h, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.scheduler.Tick(ctx, time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("completed occurrence replayed", calls)
	}
	bad := localhost.ManagementRequest{Operation: "schedule.list", Fields: map[string]json.RawMessage{"operation": json.RawMessage(`"schedule.list"`), "token": json.RawMessage(`"untrusted"`)}}
	if _, err = c.ManageLocal(ctx, bad); err == nil {
		t.Fatal("unknown management field accepted")
	}
}
