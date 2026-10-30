package deerflow

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestArchivePreservesHistoryAssetsCheckpointsAndBudget(t *testing.T) {
	ctx := context.Background()
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 5, InactiveDays: 5, CheckInterval: time.Hour}
	modelCalls := 0
	dataDir := t.TempDir()
	c, err := Open(ctx, Config{DataDir: dataDir, Retention: policy, Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		modelCalls++
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
	path := filepath.Join(x.CWD, "keep.txt")
	if err = os.WriteFile(path, []byte("original attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := (&url.URL{Scheme: "file", Path: uriPath}).String()
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "resource_link", URI: uri, Name: "keep.txt"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var runID, assetPath string
	if err = c.store.DB().QueryRow(`SELECT id FROM harness_runs WHERE session_id=?`, x.ID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow(`SELECT path FROM harness_assets WHERE session_id=?`, x.ID).Scan(&assetPath); err != nil {
		t.Fatal(err)
	}
	if err = c.store.Set(ctx, "harness/turn/v1/"+runID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	queries := []string{
		`SELECT count(*) FROM harness_runs WHERE session_id=?`,
		`SELECT count(*) FROM harness_events WHERE session_id=?`,
		`SELECT count(*) FROM harness_assets WHERE session_id=?`,
		`SELECT count(*) FROM budget_roots WHERE session_id=?`,
		`SELECT count(*) FROM budget_attempts WHERE session_id=?`,
	}
	counts := make([]int, len(queries))
	for i, query := range queries {
		if err = c.store.DB().QueryRow(query, x.ID).Scan(&counts[i]); err != nil || counts[i] == 0 {
			t.Fatalf("missing fixture state %s: %d %v", query, counts[i], err)
		}
	}
	if err = c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -6).Format(time.RFC3339Nano)
	if _, err = c.store.DB().Exec(`UPDATE harness_sessions SET closed_at=?,updated_at=? WHERE id=?`, old, old, x.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := c.ArchiveExpiredSessions(ctx); err != nil || count != 1 {
		t.Fatalf("archive: %d %v", count, err)
	}
	for i, query := range queries {
		var count int
		if err = c.store.DB().QueryRow(query, x.ID).Scan(&count); err != nil || count != counts[i] {
			t.Fatalf("archive changed %s: %d %v", query, count, err)
		}
	}
	var checkpoint []byte
	if checkpoint, _, err = c.store.Get(ctx, "harness/turn/v1/"+runID); err != nil || string(checkpoint) != "checkpoint" {
		t.Fatalf("checkpoint: %q %v", checkpoint, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("workspace file removed: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(dataDir, "assets", assetPath)); err != nil || string(content) != "original attachment" {
		t.Fatalf("attachment snapshot changed: %q %v", content, err)
	}
	if _, err = c.LoadSession(ctx, x.ID, x.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	if modelCalls != 1 {
		t.Fatalf("opening an archive replayed execution: %d", modelCalls)
	}
	var archived string
	if err = c.store.DB().QueryRow(`SELECT archived_at FROM harness_sessions WHERE id=?`, x.ID).Scan(&archived); err != nil || archived != "" {
		t.Fatalf("archive not reopened: %q %v", archived, err)
	}
}

func TestRetentionSweepArchivesExpiredDetachedSessions(t *testing.T) {
	ctx := context.Background()
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 0, InactiveDays: 30, CheckInterval: time.Hour}
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Retention: policy, Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	closed, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, closed.ID); err != nil {
		t.Fatal(err)
	}
	inactive, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, inactive.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err = c.store.DB().ExecContext(ctx, `UPDATE harness_sessions SET closed_at='',updated_at=? WHERE id=?`, old, inactive.ID); err != nil {
		t.Fatal(err)
	}
	attached, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, attached.ID); err != nil {
		t.Fatal(err)
	}
	blocked, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, blocked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `UPDATE harness_sessions SET closed_at='',updated_at=? WHERE id=?`, old, blocked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES('retention-blocked-run',?,'retention-blocked-input','completed',?,?)`, blocked.ID, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES('retention-blocked-run','tool',?,'started',1,x'7b7d')`, blocked.ID); err != nil {
		t.Fatal(err)
	}
	list, err := c.ManageLocal(ctx, localManagementRequest("session.list"))
	if err != nil {
		t.Fatal(err)
	}
	eligible := map[string]bool{}
	for _, item := range list.(map[string]any)["sessions"].([]map[string]any) {
		eligible[item["session_id"].(string)] = item["cleanup_eligible"].(bool)
	}
	if !eligible[closed.ID] || !eligible[inactive.ID] || eligible[attached.ID] || eligible[blocked.ID] {
		t.Fatalf("retention inventory=%v", eligible)
	}
	archived, err := c.CleanupExpiredSessions(ctx)
	if err != nil || archived != 2 {
		t.Fatalf("retention sweep=%v %v", archived, err)
	}
	for _, id := range []string{closed.ID, inactive.ID} {
		var archivedAt string
		if err = c.store.DB().QueryRowContext(ctx, `SELECT archived_at FROM harness_sessions WHERE id=?`, id).Scan(&archivedAt); err != nil || archivedAt == "" {
			t.Fatalf("archive marker missing: %s %v", id, err)
		}
		if _, err = c.service.Store.Session(ctx, id); err != nil {
			t.Fatalf("archived session %s lost: %v", id, err)
		}
	}
	if _, err = c.service.Store.Session(ctx, attached.ID); err != nil {
		t.Fatalf("attached session archived: %v", err)
	}
	if _, err = c.service.Store.Session(ctx, blocked.ID); err != nil {
		t.Fatalf("unreconciled session archived: %v", err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `UPDATE harness_tool_receipts SET state='completed' WHERE session_id=?`, blocked.ID); err != nil {
		t.Fatal(err)
	}
	archived, err = c.CleanupExpiredSessions(ctx)
	if err != nil || archived != 1 {
		t.Fatalf("reconciled retention sweep=%v %v", archived, err)
	}
	if _, err = c.service.Store.Session(ctx, blocked.ID); err != nil {
		t.Fatalf("reconciled session lost: %v", err)
	}
	if err = c.CloseSession(ctx, attached.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.LoadSession(ctx, closed.ID, filepath.Clean(workspace), false, nil)
	if err != nil || loaded.ID != closed.ID {
		t.Fatalf("reopen closed session: %+v %v", loaded, err)
	}
	var closedAt, archivedAt string
	if err = c.store.DB().QueryRowContext(ctx, `SELECT closed_at,archived_at FROM harness_sessions WHERE id=?`, closed.ID).Scan(&closedAt, &archivedAt); err != nil || closedAt != "" || archivedAt != "" {
		t.Fatalf("load did not reopen session: %q %v", closedAt, err)
	}
}

func TestRetentionRechecksCandidateAfterSessionRevives(t *testing.T) {
	ctx := context.Background()
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Hour}
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Retention: policy, Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
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
	old := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err = c.store.DB().ExecContext(ctx, `UPDATE harness_sessions SET closed_at='',updated_at=? WHERE id=?`, old, x.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ids, _, err := c.service.Store.ExpiredSessionIDsPage(ctx, now, policy, "", 256)
	if err != nil || len(ids) != 1 || ids[0] != x.ID {
		t.Fatalf("candidate=%v %v", ids, err)
	}
	if _, err = c.LoadSession(ctx, x.ID, x.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	removed, err := c.service.DeleteExpiredSession(ctx, x.ID, now, policy)
	if err != nil || removed {
		t.Fatalf("revived candidate was archived: %v %v", removed, err)
	}
	if _, err = c.service.Store.Session(ctx, x.ID); err != nil {
		t.Fatalf("revived session missing: %v", err)
	}
}
