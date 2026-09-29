package deerflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestRetentionSweepDeletesExpiredDetachedSessions(t *testing.T) {
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
	if _, err = c.store.DB().ExecContext(ctx, `INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES('retention-blocked-run','tool',?,'uncertain',1,x'7b7d')`, blocked.ID); err != nil {
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
	deleted, err := c.CleanupExpiredSessions(ctx)
	if err != nil || deleted != 2 {
		t.Fatalf("retention sweep=%v %v", deleted, err)
	}
	for _, id := range []string{closed.ID, inactive.ID} {
		if _, err = c.service.Store.Session(ctx, id); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("expired session %s remains: %v", id, err)
		}
	}
	if _, err = c.service.Store.Session(ctx, attached.ID); err != nil {
		t.Fatalf("attached session deleted: %v", err)
	}
	if _, err = c.service.Store.Session(ctx, blocked.ID); err != nil {
		t.Fatalf("unreconciled session deleted: %v", err)
	}
	if _, err = c.store.DB().ExecContext(ctx, `UPDATE harness_tool_receipts SET state='completed' WHERE session_id=?`, blocked.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err = c.CleanupExpiredSessions(ctx)
	if err != nil || deleted != 1 {
		t.Fatalf("reconciled retention sweep=%v %v", deleted, err)
	}
	if _, err = c.service.Store.Session(ctx, blocked.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("reconciled session remains: %v", err)
	}
	if err = c.CloseSession(ctx, attached.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.LoadSession(ctx, attached.ID, filepath.Clean(workspace), false, nil)
	if err != nil || loaded.ID != attached.ID {
		t.Fatalf("reopen closed session: %+v %v", loaded, err)
	}
	var closedAt string
	if err = c.store.DB().QueryRowContext(ctx, `SELECT closed_at FROM harness_sessions WHERE id=?`, attached.ID).Scan(&closedAt); err != nil || closedAt != "" {
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
		t.Fatalf("revived candidate was deleted: %v %v", removed, err)
	}
	if _, err = c.service.Store.Session(ctx, x.ID); err != nil {
		t.Fatalf("revived session missing: %v", err)
	}
}
