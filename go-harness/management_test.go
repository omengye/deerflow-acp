package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func localManagementRequest(operation string) localhost.ManagementRequest {
	raw, _ := json.Marshal(operation)
	return localhost.ManagementRequest{Operation: operation, Fields: map[string]json.RawMessage{"operation": raw}, ActiveConnections: 2}
}

func localManagementFields(operation string, fields map[string]string) localhost.ManagementRequest {
	request := localManagementRequest(operation)
	for key, value := range fields {
		raw, _ := json.Marshal(value)
		request.Fields[key] = raw
	}
	return request
}

func managementFacts(t *testing.T, value any) []map[string]any {
	t.Helper()
	return value.(map[string]any)["memory"].(map[string]any)["facts"].([]map[string]any)
}

func TestLocalManagementMemoryAcrossDetachedSession(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	sessionFact, err := c.CreateMemoryFact(ctx, x.ID, harness.MemorySession, harness.MemoryCandidate{Content: "session preference", Category: "preference", Confidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	workspaceFact, err := c.CreateMemoryFact(ctx, x.ID, harness.MemoryWorkspace, harness.MemoryCandidate{Content: "workspace context", Category: "context", Confidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	request := localManagementFields("memory.get", map[string]string{"session_id": x.ID})
	value, err := c.ManageLocal(ctx, request)
	if err != nil || len(managementFacts(t, value)) != 2 {
		t.Fatalf("detached memory: %+v %v", value, err)
	}
	other, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreign := localManagementFields("memory.delete", map[string]string{"session_id": other.ID, "fact_id": workspaceFact.ID})
	if _, err := c.ManageLocal(ctx, foreign); err == nil {
		t.Fatal("other workspace deleted a fact")
	}
	deleted := localManagementFields("memory.delete", map[string]string{"session_id": x.ID, "fact_id": workspaceFact.ID})
	value, err = c.ManageLocal(ctx, deleted)
	if err != nil || len(managementFacts(t, value)) != 1 || managementFacts(t, value)[0]["id"] != sessionFact.ID {
		t.Fatalf("memory deletion: %+v %v", value, err)
	}
	status, err := c.ManageLocal(ctx, localManagementRequest("daemon.status"))
	if err != nil || status.(map[string]any)["draining"] != false {
		t.Fatalf("memory deletion left drain enabled: %+v %v", status, err)
	}
}

func TestLocalManagementMemoryDeleteWaitsForActiveRun(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Engine: engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		close(started)
		select {
		case <-release:
			return harness.RunResult{StopReason: "end_turn"}, nil
		case <-ctx.Done():
			return harness.RunResult{}, ctx.Err()
		}
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	x, err := c.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fact, err := c.CreateMemoryFact(context.Background(), x.ID, harness.MemorySession, harness.MemoryCandidate{Content: "keep this", Category: "preference", Confidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := c.Run(context.Background(), x.ID, []harness.Content{{Type: "text", Text: "work"}}, nil, nil)
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not start")
	}
	request := localManagementFields("memory.delete", map[string]string{"session_id": x.ID, "fact_id": fact.ID})
	if _, err := c.ManageLocal(context.Background(), request); err == nil {
		t.Fatal("deleted memory while run was active")
	} else {
		var known *localhost.ManagementError
		if !errors.As(err, &known) || known.Code != "busy" {
			t.Fatalf("busy deletion error: %v", err)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := c.ManageLocal(context.Background(), request); err != nil {
		t.Fatalf("delete after run: %v", err)
	}
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
