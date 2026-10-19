package deerflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestScheduleWindowsShortTempNativeFlows(t *testing.T) {
	long := filepath.Join(testutil.CanonicalTempDir(t), "schedule workspace with long alias")
	if err := os.Mkdir(long, 0700); err != nil {
		t.Fatal(err)
	}
	short := testutil.ShortPath(t, long)
	t.Setenv("TEMP", short)
	t.Setenv("TMP", short)
	for name, run := range map[string]func(*testing.T){
		"buzz-lifecycle": TestScheduleToolsOwnershipLifecycleAndSourcePinning,
		"buzz-delivery":  TestScheduleBuzzLeaseExpiryAndDeliverySurvivesDeletion,
		"wechat":         TestScheduleWeChatConversationNativeRoundTrip,
		"owner-link":     TestScheduleExplicitCrossChannelOwnerLink,
	} {
		t.Run(name, run)
	}
}

func TestScheduleBuzzShortPathManagementPinsSessionWorkspace(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := testutil.CanonicalTempDir(t)
	short := testutil.ShortPath(t, workspace)
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	register := func(path string) string {
		t.Helper()
		v, err := scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.register", "account": "bot", "workspace": path, "allowed_senders": []string{"alice"}, "full_access": true})
		if err != nil {
			t.Fatal(err)
		}
		return v.(map[string]any)["version"].(string)
	}
	grant := register(short)
	if register(workspace) != grant {
		t.Fatal("an alias spelling revoked an unchanged local grant")
	}
	source := automation.Source{Kind: "buzz", Account: "bot", Sender: "alice", Workspace: short, Conversation: "private", MessageID: "message", Grant: grant}
	if _, err = scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.bind", "session_id": x.ID, "source": source}); err != nil {
		t.Fatal(err)
	}
	bound, err := c.scheduleSource(ctx, harness.RunRequest{Session: x, RunID: "pinned-alias"})
	if err != nil || bound.Workspace != x.CWD || !session.SameWorkspace(bound.Workspace, workspace) {
		t.Fatal("source did not use the pinned session workspace", bound, err)
	}
	if err = c.authorizeScheduleSource(ctx, bound, true); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{Name: "alias delivery", Prompt: "check", Workspace: x.CWD, Source: &bound, Notify: &automation.NotificationTarget{Channel: "buzz", Account: "bot", Sender: "alice", Conversation: "private"}, Kind: "once", At: time.Now().Add(time.Hour)}
	task, err := c.scheduler.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.scheduler.Tick(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	poll, err := scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.poll", "account": "bot", "workspace": short})
	if err != nil || len(poll.([]scheduleDelivery)) != 1 {
		t.Fatal("short-path polling lost the canonical delivery", poll, err)
	}
	delivery := poll.([]scheduleDelivery)[0]
	if delivery.ScheduleID != task.Spec.ID {
		t.Fatal(delivery)
	}
	for _, operation := range []string{"schedule.buzz.claim", "schedule.buzz.ack"} {
		if _, err = scheduleManageTest(t, c, map[string]any{"operation": operation, "account": "bot", "workspace": short, "id": delivery.ID, "status": "delivered"}); err != nil {
			t.Fatal("short-path delivery failed", operation, err)
		}
	}
	linked := automation.Source{Kind: "wechat", Account: "wechat-bot", Sender: "owner", Workspace: short}
	if _, err = scheduleManageTest(t, c, map[string]any{"operation": "schedule.owner.link", "sources": []automation.Source{source, linked}, "owner": "owner"}); err != nil {
		t.Fatal(err)
	}
	linked.Workspace = x.CWD
	if !c.ownsSchedule(ctx, linked, task.Spec) {
		t.Fatal("owner link did not resolve the management path alias")
	}
	source.Workspace = testutil.CanonicalTempDir(t)
	if _, err = scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.bind", "session_id": x.ID, "source": source}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("another workspace inherited the session grant", err)
	}
}
