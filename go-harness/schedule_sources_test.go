package deerflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestScheduleSourceSymlinkCannotRetargetPinnedGrant(t *testing.T) {
	ctx := context.Background()
	workspace, other := testutil.CanonicalTempDir(t), testutil.CanonicalTempDir(t)
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	v, err := scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.register", "account": "bot", "workspace": alias, "allowed_senders": []string{"alice"}, "full_access": true})
	if err != nil {
		t.Fatal(err)
	}
	source := automation.Source{Kind: "buzz", Account: "bot", Sender: "alice", Workspace: alias, Conversation: "private", MessageID: "message", Grant: v.(map[string]any)["version"].(string)}
	if err := c.bindScheduleSource(ctx, x.ID, source); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	bound, err := c.scheduleSource(ctx, harness.RunRequest{Session: x, RunID: "after-retarget"})
	if err != nil || bound.Workspace != x.CWD || bound.Workspace != workspace {
		t.Fatal("saved source followed retargeted symlink", bound, err)
	}
	if err := c.authorizeScheduleSource(ctx, bound, true); err != nil {
		t.Fatal("pinned original workspace lost its grant", err)
	}
	if err := c.bindScheduleSource(ctx, x.ID, source); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("retargeted alias rebound the original session", err)
	}
	y, err := c.NewSession(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bindScheduleSource(ctx, y.ID, source); err != nil {
		t.Fatal(err)
	}
	foreign, err := c.scheduleSource(ctx, harness.RunRequest{Session: y, RunID: "foreign-workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.authorizeScheduleSource(ctx, foreign, true); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("retargeted workspace inherited original grant", err)
	}
}
