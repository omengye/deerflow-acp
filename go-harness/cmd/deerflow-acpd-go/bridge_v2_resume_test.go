package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A no-replay resume must not enqueue history in the daemon merely to discard
// it in the Bridge: that can overflow the bounded transport write queue.
func TestExistingRustBridgeV2ResumeLargeHistoryWithoutReplay(t *testing.T) {
	bridge := os.Getenv("DEERFLOW_TEST_BRIDGE")
	if bridge == "" || testing.Short() {
		t.Skip("set DEERFLOW_TEST_BRIDGE to a native Bridge executable")
	}
	dataDir, runtimeDir, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	daemon := launchDaemon(t, dataDir, runtimeDir, "http://127.0.0.1:1/v1")
	ep := daemon.endpoint(t, runtimeDir)
	client := launchBridgeV2(t, bridge, runtimeDir)
	initialize := map[string]any{"protocolVersion": 2, "capabilities": map[string]any{}, "info": map[string]string{"name": "resume-regression", "version": "1"}}
	client.request(t, "initialize", initialize)
	raw := client.request(t, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.SessionID == "" {
		t.Fatalf("create session: %s %v", raw, err)
	}
	client.waitState(t, created.SessionID, "idle")
	client.close(t)

	// Seed only this test's private database; no model or production data is used.
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	event, err := json.Marshal(map[string]any{"sessionId": created.SessionID, "runId": "history-fixture", "kind": "text_delta", "text": "historical chunk"})
	if err != nil {
		t.Fatal(err)
	}
	for range 4096 {
		if _, err = tx.Exec("INSERT INTO harness_events(session_id,run_id,event) VALUES(?,?,?)", created.SessionID, "history-fixture", event); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	resumed := launchBridgeV2(t, bridge, runtimeDir)
	resumed.request(t, "initialize", initialize)
	resumed.request(t, "session/resume", map[string]any{"sessionId": created.SessionID, "cwd": workspace, "mcpServers": []any{}})
	resumed.waitState(t, created.SessionID, "idle")
	if len(resumed.seen) != 1 {
		t.Fatalf("no-replay resume emitted %d updates, want only ready state", len(resumed.seen))
	}
	resumed.close(t)
	conn, _, line := connectCommand(t, ep, "STOP")
	_ = conn.Close()
	if line != "OK" {
		t.Fatal(line)
	}
	daemon.wait(t, true)
}
