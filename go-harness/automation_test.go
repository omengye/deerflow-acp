package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
		if r.Session.ApprovalMode != harness.ApprovalAllowAlways {
			t.Error("schedule did not persist explicit default Full Access")
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

func TestScheduleTimingManagementContract(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		t.Error("management operations must not invoke a model")
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(input map[string]any) (any, error) {
		raw, _ := json.Marshal(input)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		return c.ManageLocal(ctx, localhost.ManagementRequest{Operation: input["operation"].(string), Fields: fields})
	}
	checkError := func(input map[string]any, code string) {
		t.Helper()
		_, err := call(input)
		var management *localhost.ManagementError
		if !errors.As(err, &management) || management.Code != code {
			t.Fatalf("wanted %s, got %v", code, err)
		}
	}
	prompt := strings.Repeat("unchanged prompt ", 900)
	created, err := call(map[string]any{"operation": "schedule.create", "spec": automation.Spec{Name: "UI test", Workspace: t.TempDir(), Prompt: prompt, Kind: "interval", EverySeconds: 3600, Approval: "read_only"}})
	if err != nil {
		t.Fatal(err)
	}
	v := created.(automation.Schedule)
	timing := map[string]any{"kind": "cron", "cron": "0 9 * * 1-5", "timezone": "Asia/Shanghai"}
	preview, err := call(map[string]any{"operation": "schedule.preview", "timing": timing})
	if err != nil || preview.(automation.Preview).NextLocal == "" {
		t.Fatal(preview, err)
	}
	checkError(map[string]any{"operation": "schedule.delete", "id": v.Spec.ID}, "invalid_request")
	checkError(map[string]any{"operation": "schedule.pause", "id": v.Spec.ID, "expected_revision": 1}, "invalid_request")
	checkError(map[string]any{"operation": "schedule.reschedule", "id": v.Spec.ID, "expected_revision": 1, "timing": map[string]any{"kind": "interval", "every_seconds": 600, "approval": "allow_always"}}, "invalid_request")
	checkError(map[string]any{"operation": "schedule.reschedule", "id": v.Spec.ID, "expected_revision": 1, "timing": map[string]any{"kind": "once", "at": "2026-12-01T09:00:00"}}, "invalid_request")
	changed, err := call(map[string]any{"operation": "schedule.reschedule", "id": v.Spec.ID, "expected_revision": 1, "timing": timing})
	if err != nil {
		t.Fatal(err)
	}
	updated := changed.(automation.Schedule)
	if updated.Revision != 2 || updated.Spec.Approval != "read_only" || updated.Spec.Prompt != prompt || updated.Spec.Workspace != v.Spec.Workspace {
		t.Fatal("timing edit changed task authority", updated)
	}
	checkError(map[string]any{"operation": "schedule.reschedule", "id": v.Spec.ID, "expected_revision": 1, "timing": timing}, "conflict")
	checkError(map[string]any{"operation": "schedule.delete", "id": "missing", "expected_revision": 1}, "not_found")
	snapshot, err := call(map[string]any{"operation": "schedule.snapshot", "id": v.Spec.ID})
	if err != nil {
		t.Fatal(err)
	}
	data := snapshot.(map[string]any)
	rows := data["items"].([]automation.Schedule)
	if len(rows) != 1 || rows[0].Revision != 2 || len(rows[0].Spec.Prompt) > 515 || data["status"].(map[string]any)["reschedule_version"] != 1 {
		t.Fatal(data)
	}
	if _, err = call(map[string]any{"operation": "schedule.pause", "id": v.Spec.ID, "expected_revision": 2, "paused": true}); err != nil {
		t.Fatal(err)
	}
	if _, err = call(map[string]any{"operation": "schedule.delete", "id": v.Spec.ID, "expected_revision": 3}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleExplicitGrantsSurviveRescheduleAndReopen(t *testing.T) {
	for _, approval := range []string{"read_only", "ask", "allow_always"} {
		t.Run(approval, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			cfg := Config{DataDir: t.TempDir(), Engine: engineFunc(func(_ context.Context, r harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				calls++
				if r.Session.ApprovalMode != approval {
					t.Errorf("got permission %q, want %q", r.Session.ApprovalMode, approval)
				}
				return harness.RunResult{StopReason: "end_turn"}, nil
			})}
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			v, err := c.scheduler.Create(ctx, automation.Spec{Name: "preserve grant", Workspace: t.TempDir(), Prompt: "inspect", Kind: "once", At: time.Now().Add(time.Hour), Approval: approval})
			if err != nil {
				c.Close()
				t.Fatal(err)
			}
			_, err = c.scheduler.Reschedule(ctx, v.Spec.ID, v.Revision, automation.Timing{Kind: "once", At: time.Now().Add(2 * time.Hour)}, false)
			if err != nil {
				c.Close()
				t.Fatal(err)
			}
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err = c.scheduler.Tick(ctx, time.Now().Add(90*time.Minute)); err != nil || calls != 0 {
				t.Fatal("old timing fired", calls, err)
			}
			if err = c.scheduler.Tick(ctx, time.Now().Add(3*time.Hour)); err != nil || calls != 1 {
				t.Fatal(calls, err)
			}
		})
	}
}
