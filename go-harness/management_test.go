package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

func TestLocalManagementDeletesDetachedSessionAndPrivateState(t *testing.T) {
	ctx := context.Background()
	dataDir, workspace := t.TempDir(), t.TempDir()
	c, err := Open(ctx, Config{DataDir: dataDir, Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	request := localManagementFields("session.delete", map[string]string{"session_id": x.ID})
	if _, err = c.ManageLocal(ctx, request); !errors.Is(err, harness.ErrBusy) {
		var known *localhost.ManagementError
		if !errors.As(err, &known) || known.Code != "busy" {
			t.Fatalf("attached deletion: %v", err)
		}
	}
	if _, err = c.CreateMemoryFact(ctx, x.ID, harness.MemorySession, harness.MemoryCandidate{Content: "private", Category: "preference", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.CreateMemoryFact(ctx, x.ID, harness.MemoryWorkspace, harness.MemoryCandidate{Content: "shared", Category: "context", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	var sessionScopeKey string
	if err = c.store.DB().QueryRowContext(ctx, `SELECT key FROM memory_scopes WHERE kind='session' AND subject=?`, x.ID).Scan(&sessionScopeKey); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "notes.txt")
	if err = os.WriteFile(path, []byte("snapshot content"), 0600); err != nil {
		t.Fatal(err)
	}
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := (&url.URL{Scheme: "file", Path: uriPath}).String()
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "resource_link", URI: uri, Name: "notes.txt"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var runID, assetPath string
	if err = c.store.DB().QueryRowContext(ctx, `SELECT id FROM harness_runs WHERE session_id=?`, x.ID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRowContext(ctx, `SELECT path FROM harness_assets WHERE session_id=?`, x.ID).Scan(&assetPath); err != nil {
		t.Fatal(err)
	}
	if err = c.store.Set(ctx, "harness/turn/v1/"+runID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	value, err := c.ManageLocal(ctx, request)
	if err != nil || value.(map[string]any)["already_deleted"] != false {
		t.Fatalf("delete: %+v %v", value, err)
	}
	for _, query := range []string{
		`SELECT count(*) FROM harness_sessions WHERE id=?`,
		`SELECT count(*) FROM harness_runs WHERE session_id=?`,
		`SELECT count(*) FROM harness_events WHERE session_id=?`,
		`SELECT count(*) FROM eino_session_events WHERE session_id=?`,
		`SELECT count(*) FROM harness_assets WHERE session_id=?`,
		`SELECT count(*) FROM harness_artifacts WHERE session_id=?`,
		`SELECT count(*) FROM budget_roots WHERE session_id=?`,
		`SELECT count(*) FROM budget_members WHERE session_id=?`,
		`SELECT count(*) FROM budget_attempts WHERE session_id=?`,
		`SELECT count(*) FROM budget_reservations WHERE session_id=?`,
		`SELECT count(*) FROM memory_scopes WHERE kind='session' AND subject=?`,
	} {
		var n int
		if err = c.store.DB().QueryRowContext(ctx, query, x.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("private state remains (%s): %d %v", query, n, err)
		}
	}
	var n int
	if err = c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_scopes WHERE kind='workspace'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("shared memory removed: %d %v", n, err)
	}
	for _, query := range []string{`SELECT count(*) FROM memory_facts`, `SELECT count(*) FROM memory_fact_revisions`} {
		if err = c.store.DB().QueryRowContext(ctx, query).Scan(&n); err != nil || n != 1 {
			t.Fatalf("private memory remains or shared memory disappeared (%s): %d %v", query, n, err)
		}
	}
	if err = c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_facts_fts WHERE scope_key=?`, sessionScopeKey).Scan(&n); err != nil || n != 0 {
		t.Fatalf("session FTS rows remain: %d %v", n, err)
	}
	if err = c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+runID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("checkpoint remains: %d %v", n, err)
	}
	if _, err = os.Stat(filepath.Join(dataDir, "assets", assetPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot remains: %v", err)
	}
	check, err := c.store.DB().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if check.Next() {
		check.Close()
		t.Fatal("delete left a foreign key violation")
	}
	if err = check.Err(); err != nil {
		t.Fatal(err)
	}
	check.Close()
	value, err = c.ManageLocal(ctx, request)
	if err != nil || value.(map[string]any)["already_deleted"] != true {
		t.Fatalf("idempotent delete: %+v %v", value, err)
	}
}

func TestLocalManagementDeleteRejectsBackgroundAndUnresolvedReceipt(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "one"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var runID string
	if err = c.store.DB().QueryRowContext(ctx, `SELECT id FROM harness_runs WHERE session_id=?`, x.ID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	request := localManagementFields("session.delete", map[string]string{"session_id": x.ID})
	// The custom engine leaves background workers disabled. Install their
	// persistent binding schema to exercise the deletion guard directly.
	if _, err = c.store.DB().ExecContext(ctx, `CREATE TABLE harness_background_bindings (task_id TEXT PRIMARY KEY REFERENCES eino_background_tasks(id),parent_session_id TEXT NOT NULL,child_session_id TEXT NOT NULL,origin_run_id TEXT NOT NULL,origin_tool_call_id TEXT NOT NULL,intent_hash TEXT NOT NULL,payload BLOB NOT NULL,blocked_reason TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `INSERT INTO eino_background_tasks(id,executor_key,status,version,payload) VALUES('task-fixture','fixture','completed',1,x'00')`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `INSERT INTO harness_background_bindings(task_id,parent_session_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload) VALUES('task-fixture',?,'child-fixture',?,'call-fixture','hash',x'00')`, x.ID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ManageLocal(ctx, request); err == nil {
		t.Fatal("background parent was deleted")
	}
	if _, err = c.store.DB().ExecContext(ctx, `DELETE FROM harness_background_bindings WHERE task_id='task-fixture'`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `DELETE FROM eino_background_tasks WHERE id='task-fixture'`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,'call-fixture',?,'uncertain',1,x'7b7d')`, runID, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ManageLocal(ctx, request); err == nil {
		t.Fatal("unresolved receipt was deleted")
	}
	if _, err = c.store.DB().ExecContext(ctx, `UPDATE harness_tool_receipts SET state='completed' WHERE run_id=?`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ManageLocal(ctx, request); err != nil {
		t.Fatal(err)
	}
}

func TestLocalManagementDeleteFailureRollsBackAndCanRetry(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `CREATE TRIGGER deny_purge BEFORE DELETE ON harness_sessions BEGIN SELECT RAISE(ABORT,'fixture purge failure'); END`); err != nil {
		t.Fatal(err)
	}
	request := localManagementFields("session.delete", map[string]string{"session_id": x.ID})
	if _, err = c.ManageLocal(ctx, request); err == nil {
		t.Fatal("failed purge reported success")
	}
	if _, err = c.store.DB().ExecContext(ctx, `DROP TRIGGER deny_purge`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, x.ID, x.CWD, false, nil); err != nil {
		t.Fatalf("failed purge left a reconnect fence: %v", err)
	}
	if err = c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ManageLocal(ctx, request); err != nil {
		t.Fatal(err)
	}
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
