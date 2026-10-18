package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestChannelNewCreatesEmptySessionWithoutModelAndPreservesOld(t *testing.T) {
	workspace := testutil.CanonicalTempDir(t)
	checkChannelNewSession(t, workspace, workspace)
}

func checkChannelNewSession(t *testing.T, workspace, canonicalWorkspace string) {
	t.Helper()
	ctx := context.Background()
	calls := 0
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls++
		return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "answer"})
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var current string
	bind := func(id string) error { current = id; return nil }
	old, err := c.channelRun(ctx, "", workspace, "hello", harness.ApprovalGlobal, bind)
	if err != nil || calls != 1 {
		t.Fatal(old, err, calls)
	}
	history, err := c.service.Store.History(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	created, err := c.channelRun(ctx, current, workspace, " \n/new\t", harness.ApprovalGlobal, bind)
	if err != nil || created.Status != "completed" || created.SessionID == old.SessionID || created.SessionID != current || calls != 1 {
		t.Fatal(created, err, calls)
	}
	x, err := c.service.Store.Session(ctx, current)
	if err != nil || x.ApprovalMode != harness.ApprovalGlobal || x.CWD != canonicalWorkspace {
		t.Fatalf("new session=%+v want canonical cwd=%q (input=%q) err=%v", x, canonicalWorkspace, workspace, err)
	}
	var n int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_runs WHERE session_id=?`, current).Scan(&n); err != nil || n != 0 {
		t.Fatal("/new submitted a prompt", n, err)
	}
	retained, err := c.service.Store.History(ctx, old.SessionID)
	if err != nil || len(retained) != len(history) {
		t.Fatal("old history changed", err)
	}
	if _, err = c.channelRun(ctx, current, workspace, "/new explain this command", harness.ApprovalGlobal, bind); err != nil || calls != 2 {
		t.Fatal("non-exact command intercepted", err, calls)
	}
	unchanged := current
	failed, err := c.channelRun(ctx, current, workspace, "/new", harness.ApprovalGlobal, func(string) error { return errors.New("bind failure") })
	if err == nil || failed.Status == "completed" || current != unchanged || calls != 2 {
		t.Fatal("failed binding reported success", failed, err)
	}
}

func TestChannelToolFeedbackBoundedAndRedacted(t *testing.T) {
	var tools channelTools
	secret := "DO-NOT-FORWARD-SECRET"
	for i := range 12 {
		id := fmt.Sprint(i)
		_ = tools.observe(context.Background(), harness.RunEvent{Kind: "tool_start", ToolCallID: id, ToolName: "web_fetch", Arguments: json.RawMessage(`{"url":"https://user:` + secret + `@example.com/secret?token=` + secret + `","headers":{"Authorization":"` + secret + `"}}`), Text: secret})
		_ = tools.observe(context.Background(), harness.RunEvent{Kind: "tool_end", ToolCallID: id, ToolName: "web_fetch", Status: "completed", Content: []harness.Content{{Text: secret}}})
	}
	_ = tools.observe(context.Background(), harness.RunEvent{Kind: "tool_start", ToolCallID: "pending", ToolName: "web_fetch"})
	state := &harness.ExecutionState{Status: harness.ExecutionWaitingInput, Resumable: true, WaitingInputs: []harness.ExecutionInteraction{{ToolCallID: "pending", ToolName: "web_fetch", ArgumentsSummary: secret}, {ToolName: secret}}}
	feedback := tools.summary(state) + channelAttention("session", state, errors.New(secret))
	if strings.Contains(feedback, secret) || strings.Contains(feedback, "example.com") || !strings.Contains(feedback, "最近 8 项") || !strings.Contains(feedback, "等待确认") || !strings.Contains(feedback, "已完成") || len(feedback) > 4000 {
		t.Fatal("unsafe or missing feedback", feedback)
	}
	if got := channelAttention("session", nil, errors.New(secret)); strings.Contains(got, secret) {
		t.Fatal(got)
	}
}
