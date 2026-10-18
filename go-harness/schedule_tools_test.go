package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func scheduleManageTest(t *testing.T, c *Client, request map[string]any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(request)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return c.ManageLocal(context.Background(), localhost.ManagementRequest{Operation: request["operation"].(string), Fields: fields})
}

// A real Eino HTTP model request must advertise the scheduling capability and
// persist an actual tool receipt. Direct schedule.create smoke is insufficient.
func TestScheduleNaturalLanguageNativeTools(t *testing.T) {
	for _, policy := range []string{harness.ApprovalAllowAlways, harness.ApprovalReadOnly} {
		t.Run(policy, func(t *testing.T) {
			var mu sync.Mutex
			var calls int
			var receipt string
			at := time.Now().Add(time.Hour)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream   bool
					Messages []struct{ Role, Content string }
					Tools    []struct{ Function struct{ Name string } }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				calls++
				foundRead, foundWrite, instructions := false, false, false
				for _, tool := range req.Tools {
					foundRead = foundRead || tool.Function.Name == "schedule_read"
					foundWrite = foundWrite || tool.Function.Name == "schedule_manage"
				}
				for _, m := range req.Messages {
					if m.Role == "system" {
						instructions = instructions || strings.Contains(m.Content, "supports native scheduled tasks")
					}
					if m.Role == "tool" {
						receipt = m.Content
					}
				}
				if !foundRead || !foundWrite || !instructions {
					t.Error("missing native scheduling capability in actual model request")
				}
				if calls == 1 {
					args, _ := json.Marshal(map[string]any{"action": "create", "request_id": "morning-check", "name": "Morning check", "prompt": "Check build result", "timing": map[string]any{"kind": "once", "at": at, "timezone": "Asia/Shanghai"}})
					skillToolResponse(w, req.Stream, "schedule_manage", string(args), "")
					return
				}
				skillToolResponse(w, req.Stream, "", "", "Checked native schedule receipt")
			}))
			defer server.Close()
			c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: server.URL + "/v1", PermissionMode: harness.PermissionModeDangerous, DisableSubagents: true})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			x, err := c.NewSession(context.Background(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.SetConfigOption(context.Background(), x.ID, "approval", policy); err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(context.Background(), x.ID, []harness.Content{{Type: "text", Text: "Create a reminder to check the build in one hour."}}, nil, nil)
			if err != nil {
				t.Fatal(result, err)
			}
			items, err := c.scheduler.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if policy == harness.ApprovalReadOnly {
				if len(items) != 0 {
					t.Fatal("readonly escalated", items)
				}
				return
			}
			if len(items) != 1 || items[0].Spec.Approval != harness.ApprovalAllowAlways || items[0].Spec.Source.Kind != "desktop" || !strings.Contains(receipt, items[0].Spec.ID) || !strings.Contains(receipt, "next_local") {
				t.Fatal("missing persisted full-access receipt", items, receipt)
			}
		})
	}
}

func TestScheduleToolsOwnershipLifecycleAndSourcePinning(t *testing.T) {
	ctx := context.Background()
	var c *Client
	phase := "create"
	var created automation.Schedule
	var source automation.Source
	var originalRun harness.RunRequest
	var grant string
	engine := engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if phase == "scheduled" {
			list, _, err := c.scheduleTools(ctx, req)
			if err != nil || len(list) != 0 {
				t.Fatal("scheduled execution acquired recursive scheduler tools", err)
			}
			return harness.RunResult{StopReason: "end_turn"}, nil
		}
		list, _, err := c.scheduleTools(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		tools := backgroundToolsByName(t, list)
		invoke := func(name string, input any) string {
			raw, _ := json.Marshal(input)
			out, err := tools[name].InvokableRun(ctx, string(raw))
			if err != nil {
				t.Fatal(phase, err)
			}
			return out
		}
		switch phase {
		case "create":
			originalRun = req
			input := map[string]any{"action": "create", "request_id": "one", "name": "Buzz-owned", "prompt": "inspect", "timing": map[string]any{"kind": "interval", "every_seconds": 3600, "timezone": "Asia/Shanghai"}}
			out := invoke("schedule_manage", input)
			if err = json.Unmarshal([]byte(out), &created); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(invoke("schedule_manage", input), created.Spec.ID) {
				t.Fatal("duplicate create")
			}
			if created.Spec.Source.Sender != "alice" || created.Spec.Notify.Conversation != "private" || created.Spec.Notify.ReplyTo != "message-1" {
				t.Fatal(created)
			}
			if _, err = tools["schedule_manage"].InvokableRun(ctx, `{"action":"create","workspace":"/other"}`); err == nil {
				t.Fatal("untrusted authority accepted")
			}
		case "other":
			if out := invoke("schedule_read", map[string]any{"action": "list"}); out != "[]" {
				t.Fatal("foreign tasks leaked", out)
			}
			for _, action := range []string{"get", "history"} {
				if _, err = tools["schedule_read"].InvokableRun(ctx, `{"action":"`+action+`","id":"`+created.Spec.ID+`"}`); !errors.Is(err, automation.ErrNotFound) {
					t.Fatal(err)
				}
			}
		case "owner":
			if !strings.Contains(invoke("schedule_read", map[string]any{"action": "list"}), created.Spec.ID) {
				t.Fatal("new session lost ownership")
			}
			out := invoke("schedule_manage", map[string]any{"action": "pause", "id": created.Spec.ID, "expected_revision": created.Revision})
			_ = json.Unmarshal([]byte(out), &created)
			out = invoke("schedule_manage", map[string]any{"action": "reschedule", "id": created.Spec.ID, "expected_revision": created.Revision, "timing": map[string]any{"kind": "cron", "cron": "0 9 * * 1-5", "timezone": "Asia/Shanghai"}})
			_ = json.Unmarshal([]byte(out), &created)
			if !created.Paused {
				t.Fatal("edit resumed task")
			}
			out = invoke("schedule_manage", map[string]any{"action": "resume", "id": created.Spec.ID, "expected_revision": created.Revision})
			_ = json.Unmarshal([]byte(out), &created)
		case "revoked":
			if _, err = tools["schedule_manage"].InvokableRun(ctx, `{"action":"delete","id":"`+created.Spec.ID+`","expected_revision":1}`); !errors.Is(err, harness.ErrPermissionDenied) {
				t.Fatal("grant bypass", err)
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	var err error
	c, err = Open(ctx, Config{DataDir: t.TempDir(), Engine: engine})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	register := func(full bool) {
		v, err := scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.register", "account": "buzz-account", "workspace": workspace, "allowed_senders": []string{"alice", "bob"}, "full_access": full})
		if err != nil {
			t.Fatal(err)
		}
		grant = v.(map[string]any)["version"].(string)
	}
	register(true)
	run := func(sender string) {
		x, err := c.NewSession(ctx, workspace)
		if err != nil {
			t.Fatal(err)
		}
		source = automation.Source{Kind: "buzz", Account: "buzz-account", Sender: sender, Workspace: workspace, Conversation: "private", MessageID: "message-1", SessionID: x.ID, Grant: grant}
		if err = c.bindScheduleSource(ctx, x.ID, source); err != nil {
			t.Fatal(err)
		}
		if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: phase}}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	run("alice")
	phase = "other"
	run("bob")
	phase = "owner"
	run("alice")
	// A new message/session binding cannot change the already admitted run.
	if err = c.bindScheduleSource(ctx, originalRun.Session.ID, automation.Source{Kind: "desktop", Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	pinned, err := c.scheduleSource(ctx, originalRun)
	if err != nil || pinned.Kind != "buzz" || pinned.Sender != "alice" {
		t.Fatal(pinned, err)
	}
	phase = "scheduled"
	if err = c.scheduler.Tick(ctx, time.Now().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	history, err := c.scheduler.History(ctx, created.Spec.ID)
	if err != nil || len(history) != 1 || history[0].Outcome.Status != "completed" {
		t.Fatal(history, err)
	}
	poll, err := scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.poll", "account": "buzz-account", "workspace": workspace})
	if err != nil || len(poll.([]scheduleDelivery)) != 1 {
		t.Fatal(poll, err)
	}
	delivery := poll.([]scheduleDelivery)[0]
	for _, op := range []string{"schedule.buzz.claim", "schedule.buzz.ack"} {
		_, err = scheduleManageTest(t, c, map[string]any{"operation": op, "account": "buzz-account", "workspace": workspace, "id": delivery.ID, "status": "unknown"})
		if err != nil {
			t.Fatal(err)
		}
	}
	poll, err = scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.poll", "account": "buzz-account", "workspace": workspace})
	if err != nil || len(poll.([]scheduleDelivery)) != 0 {
		t.Fatal("uncertain send replayed", poll, err)
	}
	if _, err = scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.claim", "account": "buzz-account", "workspace": workspace, "id": delivery.ID}); !errors.Is(err, automation.ErrConflict) {
		t.Fatal(err)
	}
	history, err = c.scheduleHistory(ctx, created.Spec.ID)
	if err != nil || history[0].Notification != "unknown" {
		t.Fatal("notification state missing", history, err)
	}
	register(false)
	phase = "revoked"
	run("alice")
	if err = c.scheduler.Tick(ctx, time.Now().Add(16*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	state, err := c.scheduler.Get(ctx, created.Spec.ID)
	if err != nil || !state.Paused || state.Last.Outcome.Status != "needs_attention" {
		t.Fatal(state, err)
	}
}

func TestScheduleBuzzLeaseExpiryAndDeliverySurvivesDeletion(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	register := map[string]any{"operation": "schedule.buzz.register", "account": "bot", "workspace": workspace, "allowed_senders": []string{"alice"}, "full_access": true}
	v, err := scheduleManageTest(t, c, register)
	if err != nil {
		t.Fatal(err)
	}
	source := automation.Source{Kind: "buzz", Account: "bot", Sender: "alice", Workspace: workspace, Grant: v.(map[string]any)["version"].(string)}
	_, err = c.store.DB().ExecContext(ctx, `UPDATE harness_schedule_buzz_grants SET expires_ms=0`)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.authorizeScheduleSource(context.WithValue(ctx, scheduledRunKey{}, true), source, true); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("expired adapter lease allowed dispatch", err)
	}
	v, err = scheduleManageTest(t, c, register)
	if err != nil || v.(map[string]any)["version"] != source.Grant {
		t.Fatal("ordinary reconnect revoked unchanged grant", v, err)
	}
	spec := automation.Spec{Name: "notification", Prompt: "check", Workspace: workspace, Source: &source, Notify: &automation.NotificationTarget{Channel: "buzz", Account: "bot", Sender: "alice", Conversation: "channel", ReplyTo: "message"}, Kind: "once", At: time.Now().Add(time.Hour)}
	task, err := c.scheduler.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.scheduler.Tick(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	task, err = c.scheduler.Get(ctx, task.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.scheduler.Delete(ctx, task.Spec.ID, task.Revision); err != nil {
		t.Fatal(err)
	}
	poll, err := scheduleManageTest(t, c, map[string]any{"operation": "schedule.buzz.poll", "account": "bot", "workspace": workspace})
	if err != nil || len(poll.([]scheduleDelivery)) != 1 {
		t.Fatal("deletion lost committed notification", poll, err)
	}
	id := poll.([]scheduleDelivery)[0].ID
	for _, op := range []string{"schedule.buzz.claim", "schedule.buzz.ack"} {
		if _, err = scheduleManageTest(t, c, map[string]any{"operation": op, "account": "bot", "workspace": workspace, "id": id, "status": "delivered"}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := c.scheduleHistory(ctx, task.Spec.ID)
	if err != nil || len(history) != 1 || history[0].Notification != "delivered" {
		t.Fatal(history, err)
	}
	register["full_access"] = false
	if _, err = scheduleManageTest(t, c, register); err != nil {
		t.Fatal(err)
	}
	register["full_access"] = true
	if _, err = scheduleManageTest(t, c, register); err != nil {
		t.Fatal(err)
	}
	if err = c.authorizeScheduleSource(ctx, source, true); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("revoked grant silently resurrected", err)
	}
}

func TestScheduleWeChatConversationNativeRoundTrip(t *testing.T) {
	ctx := context.Background()
	var calls int
	var mu sync.Mutex
	var created automation.Schedule
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool
			Messages []struct{ Role, Content string }
			Tools    []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			args, _ := json.Marshal(map[string]any{"action": "create", "request_id": "daily", "name": "Channel daily", "prompt": "Report the project status", "timing": map[string]any{"kind": "interval", "every_seconds": 60, "timezone": "Asia/Shanghai"}})
			skillToolResponse(w, req.Stream, "schedule_manage", string(args), "")
			return
		}
		if calls == 2 {
			for _, m := range req.Messages {
				if m.Role == "tool" {
					_ = json.Unmarshal([]byte(m.Content), &created)
				}
			}
		} else {
			for _, tool := range req.Tools {
				if tool.Function.Name == "schedule_manage" || tool.Function.Name == "schedule_read" {
					t.Error("unattended channel result can schedule recursively")
				}
			}
		}
		skillToolResponse(w, req.Stream, "", "", "Project healthy")
	}))
	defer provider.Close()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeAll, DisableSubagents: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	d := setupChannelCommandFixture(t, c, workspace)
	if _, err = c.channels.SetPermissions(ctx, d.Instance, d.Revision, harness.ApprovalAllowAlways); err != nil {
		t.Fatal(err)
	}
	d.Revision++
	d.MessageID = "message-create"
	out, err := c.channelRun(channels.WithDispatch(ctx, d), "", workspace, "Every minute report project status here", harness.ApprovalAllowAlways, func(string) error { return nil })
	if err != nil || out.Status != "completed" {
		t.Fatal(out, err)
	}
	mu.Lock()
	task := created
	mu.Unlock()
	if task.Spec.ID == "" || task.Spec.Source.Kind != d.Instance || task.Spec.Source.Sender != "alice" || task.Spec.Notify.Account != "bot" {
		t.Fatal(task)
	}
	if err = c.scheduler.Tick(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var text, status string
	if err = c.store.DB().QueryRowContext(ctx, `SELECT reply,status FROM harness_channel_messages WHERE instance=? AND id LIKE 'schedule-%'`, d.Instance).Scan(&text, &status); err != nil || !strings.Contains(text, "Project healthy") {
		t.Fatal(text, status, err)
	}
	// Message admitted without Full Access must not inherit a later upgrade.
	var pinned automation.Source
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err = c.scheduleSource(channels.WithDispatch(ctx, d), harness.RunRequest{Session: x, RunID: "old-channel-message"})
	if err != nil || pinned.Grant != "" {
		t.Fatal("queued policy upgraded", pinned, err)
	}
	if err = c.authorizeScheduleSource(ctx, pinned, true); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal(err)
	}
	if _, err = c.channels.SetPermissions(ctx, d.Instance, d.Revision, harness.ApprovalGlobal); err != nil {
		t.Fatal(err)
	}
	if err = c.scheduler.Tick(ctx, time.Now().Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, err := c.scheduler.Get(ctx, task.Spec.ID)
	if err != nil || !v.Paused || v.Last.Outcome.Status != "needs_attention" {
		t.Fatal(v, err)
	}
}

func TestScheduleExplicitCrossChannelOwnerLink(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	a := automation.Source{Kind: "buzz", Account: "bot", Sender: "alice", Workspace: workspace}
	b := automation.Source{Kind: "wechat", Account: "wechat-bot", Sender: "owner", Workspace: workspace}
	spec := automation.Spec{Source: &a, Workspace: workspace}
	if c.ownsSchedule(ctx, b, spec) {
		t.Fatal("implicit cross-channel identity")
	}
	for _, owner := range []string{"local-owner", ""} {
		if _, err = scheduleManageTest(t, c, map[string]any{"operation": "schedule.owner.link", "sources": []automation.Source{a, b}, "owner": owner}); err != nil {
			t.Fatal(err)
		}
		if c.ownsSchedule(ctx, b, spec) != (owner != "") {
			t.Fatal("link/unlink failed")
		}
	}
}

func TestScheduleDelegatedAgentCannotCreatePersistentWork(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprint("named=", named), func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream bool
					Tools  []struct{ Function struct{ Name string } }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					skillToolResponse(w, req.Stream, "task", `{"subagent_type":"general-purpose","prompt":"Try the schedule boundary","description":"isolation check"}`, "", "delegate")
					return
				}
				if calls == 2 {
					visible := false
					for _, tool := range req.Tools {
						visible = visible || tool.Function.Name == "schedule_manage"
					}
					if named && visible {
						t.Error("named role inherited schedule tools")
					}
					if visible {
						skillToolResponse(w, req.Stream, "schedule_manage", `{"action":"create","request_id":"child","name":"disallowed","prompt":"inspect","timing":{"kind":"interval","every_seconds":3600}}`, "", "child-create")
						return
					}
				}
				skillToolResponse(w, req.Stream, "", "", "Boundary checked")
			}))
			defer server.Close()
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: server.URL + "/v1", PermissionMode: harness.PermissionModeOff, MaxIterations: 8}
			if named {
				cfg.Subagents = &harness.SubagentsConfig{}
			}
			c, err := Open(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			x, err := c.NewSession(context.Background(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Run(context.Background(), x.ID, []harness.Content{{Type: "text", Text: "Test isolation with a child agent"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			items, err := c.scheduler.List(context.Background())
			if err != nil || len(items) != 0 {
				t.Fatal("child created persistent work", items, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls < 3 {
				t.Fatal("delegation not exercised", calls)
			}
		})
	}
}
