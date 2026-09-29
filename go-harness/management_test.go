package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func localManagementRequest(operation string) localhost.ManagementRequest {
	raw, _ := json.Marshal(operation)
	return localhost.ManagementRequest{Operation: operation, Fields: map[string]json.RawMessage{"operation": raw}, ActiveConnections: 2}
}

func TestManagementStatusIncludesBackgroundWork(t *testing.T) {
	status := managementStatus(session.Activity{Draining: true, ActiveOperations: 1}, background.Activity{Draining: true, ActiveOperations: 3, ActiveRuns: 2, QueuedRuns: 1}, 2)
	if status["active_operations"] != 4 || status["active_runs"] != 3 || status["queued_runs"] != 1 || status["connections"] != 2 {
		t.Fatalf("status lost background work: %+v", status)
	}
}

func TestLocalManagementStatusListAndDrain(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	value, err := c.ManageLocal(ctx, localManagementRequest("daemon.status"))
	if err != nil {
		t.Fatal(err)
	}
	status := value.(map[string]any)
	if status["draining"] != false || status["active_operations"] != 0 || status["connections"] != 2 {
		t.Fatalf("status: %+v", status)
	}
	value, err = c.ManageLocal(ctx, localManagementRequest("session.list"))
	if err != nil {
		t.Fatal(err)
	}
	listed := value.(map[string]any)["sessions"].([]map[string]any)
	if len(listed) != 1 || listed[0]["session_id"] != session.ID || listed[0]["phase"] != "attached" || listed[0]["cwd"] != workspace || listed[0]["cleanup_eligible"] != false {
		t.Fatalf("session inventory: %+v", listed)
	}
	value, err = c.ManageLocal(ctx, localManagementRequest("daemon.drain"))
	if err != nil || value.(map[string]any)["draining"] != true {
		t.Fatalf("drain: %+v %v", value, err)
	}
	if _, err := c.NewSession(ctx, workspace); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("new session admitted during drain: %v", err)
	}
	if _, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "run"}}, nil, nil); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("prompt admitted during drain: %v", err)
	}
	value, err = c.ManageLocal(ctx, localManagementRequest("session.list"))
	if err != nil || len(value.(map[string]any)["sessions"].([]map[string]any)) != 1 {
		t.Fatalf("drained new session left a row: %+v %v", value, err)
	}
	value, err = c.ManageLocal(ctx, localManagementRequest("daemon.resume"))
	if err != nil || value.(map[string]any)["draining"] != false {
		t.Fatalf("resume: %+v %v", value, err)
	}
	if _, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "run"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	invalid := localManagementRequest("daemon.status")
	invalid.Fields["extra"] = json.RawMessage(`true`)
	if _, err := c.ManageLocal(ctx, invalid); err == nil {
		t.Fatal("unknown management field accepted")
	}
}
