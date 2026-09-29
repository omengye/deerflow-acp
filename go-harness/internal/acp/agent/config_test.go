package agent_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func configValues(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var response struct {
		Options []harness.ConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, option := range response.Options {
		values[option.ID] = option.CurrentValue
	}
	return values
}

func (c *client) configure(t *testing.T, sid, key, value string) map[string]string {
	t.Helper()
	id := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": key, "value": value})
	event := update(t, c.read(t), sid, "config_option_update")
	_ = event
	values := configValues(t, c.success(t, id))
	if values[key] != value {
		t.Fatalf("configuration %s=%q want %q", key, values[key], value)
	}
	return values
}

func TestConfigurationMetadataPersistsAcrossRuntimeRestart(t *testing.T) {
	observed := make(chan harness.Session, 2)
	engine := engineFunc(func(_ context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		observed <- req.Session
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	f := newFixture(t, engine)
	f.service.Settings = hr.ConfigSettings{Models: []harness.ConfigValue{{Value: "alternate-model", Name: "Alternate"}}, EnableSubagents: true, DefaultSubagents: true}
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	values := c.configure(t, sid, "model", "alternate-model")
	if values["approval"] != "ask" || values["subagent"] != "on" || values["thinking"] != "" {
		t.Fatalf("metadata=%v", values)
	}
	c.configure(t, sid, "approval", "read_only")
	c.configure(t, sid, "subagent", "off")
	bad := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "value": "not-allowlisted"})
	if msg := c.response(t, bad); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("invalid model=%+v", msg)
	}
	id := c.request(t, "session/prompt", promptParams(sid, "inspect"))
	stopReason(t, c.success(t, id), "end_turn")
	x := <-observed
	if x.Model != "alternate-model" || x.ApprovalMode != "read_only" || x.Subagents || x.ConfigVersion != 4 {
		t.Fatalf("runtime session=%+v", x)
	}
	_ = c.in.Close()
	select {
	case <-c.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect incomplete")
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(filepath.Dir(f.cwd), "state", "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := hr.NewStore(context.Background(), db.DB())
	if err != nil {
		t.Fatal(err)
	}
	restarted := hr.NewService(store, engine, "fake-model")
	restarted.Settings = f.service.Settings
	next := connect(t, restarted)
	next.initialize(t)
	resume := next.request(t, "session/resume", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	values = configValues(t, next.success(t, resume))
	if values["model"] != "alternate-model" || values["approval"] != "read_only" || values["subagent"] != "off" {
		t.Fatalf("restored config=%v", values)
	}
}

func TestConfigurationConflictsCancelAndCachedPermissionReset(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, permission harness.PermissionHandler) (harness.RunResult, error) {
		decision, err := permission(ctx, harness.PermissionRequest{ToolCallID: "call", ToolName: "write_file", Arguments: json.RawMessage(`{"path":"x"}`)})
		if err != nil {
			return harness.RunResult{}, err
		}
		if err = emit(ctx, harness.RunEvent{Kind: "text_delta", Text: string(decision)}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	f.service.Settings.Models = []harness.ConfigValue{{Value: "other", Name: "Other"}}
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	first := c.request(t, "session/prompt", promptParams(sid, "write"))
	permission := c.read(t)
	if permission.Method != "session/request_permission" {
		t.Fatalf("permission=%+v", permission)
	}
	conflict := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": "approval", "value": "allow_always"})
	if msg := c.response(t, conflict); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("active reconfiguration=%+v", msg)
	}
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": permission.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_always"}}})
	update(t, c.read(t), sid, "agent_message_chunk")
	stopReason(t, c.success(t, first), "end_turn")
	second := c.request(t, "session/prompt", promptParams(sid, "repeat"))
	// The cached approval emits text without a second reverse request.
	update(t, c.read(t), sid, "agent_message_chunk")
	stopReason(t, c.success(t, second), "end_turn")
	c.configure(t, sid, "model", "other")
	third := c.request(t, "session/prompt", promptParams(sid, "repeat after configuration"))
	permission2 := c.read(t)
	if permission2.Method != "session/request_permission" {
		t.Fatalf("old approval survived config change: %+v", permission2)
	}
	var requestMeta struct {
		Meta struct {
			Deerflow struct {
				ConfigVersion int64 `json:"configVersion"`
			} `json:"deerflow"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(permission2.Params, &requestMeta); err != nil {
		t.Fatal(err)
	}
	if requestMeta.Meta.Deerflow.ConfigVersion != 2 {
		t.Fatalf("approval config version=%d", requestMeta.Meta.Deerflow.ConfigVersion)
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	stopReason(t, c.success(t, third), "cancelled")
	c.configure(t, sid, "approval", "reject_always")
	fourth := c.request(t, "session/prompt", promptParams(sid, "reject without asking"))
	event := update(t, c.read(t), sid, "agent_message_chunk")
	if chunkText(t, event) != "reject_once" {
		t.Fatalf("decision=%q", chunkText(t, event))
	}
	stopReason(t, c.success(t, fourth), "end_turn")
}

func TestBudgetLimitAndEstimatedUsageUseStableProtocolMetadata(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for _, e := range []harness.RunEvent{{Kind: "usage", Usage: &harness.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, Estimated: true}}, {Kind: "usage", Usage: &harness.Usage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6}}, {Kind: "budget_exhausted", Text: "run tokens budget exhausted"}} {
			if err := emit(ctx, e); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "max_tokens", Limit: "tokens"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "bounded turn"))
	event := update(t, c.read(t), sid, "agent_message_chunk")
	if chunkText(t, event) != "run tokens budget exhausted" {
		t.Fatal("budget notice missing")
	}
	raw := c.success(t, id)
	stopReason(t, raw, "max_tokens")
	var response struct {
		Meta struct {
			Deerflow struct {
				Limit string        `json:"limit"`
				Usage harness.Usage `json:"usage"`
			} `json:"deerflow"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	got := response.Meta.Deerflow
	if got.Limit != "tokens" || got.Usage.TotalTokens != 18 || got.Usage.InputTokens != 15 || !got.Usage.Estimated {
		t.Fatalf("budget metadata=%+v", got)
	}
}

func TestExplicitApprovalModesAndPlanGuard(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, permission harness.PermissionHandler) (harness.RunResult, error) {
		decision, err := permission(ctx, harness.PermissionRequest{ToolCallID: "call", ToolName: req.Input[0].Text, Arguments: json.RawMessage(`{}`)})
		if err != nil {
			return harness.RunResult{}, err
		}
		if err = emit(ctx, harness.RunEvent{Kind: "text_delta", Text: string(decision)}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	run := func(tool, want string) {
		t.Helper()
		id := c.request(t, "session/prompt", promptParams(sid, tool))
		event := update(t, c.read(t), sid, "agent_message_chunk")
		if chunkText(t, event) != want {
			t.Fatalf("tool %s decision=%s want=%s", tool, chunkText(t, event), want)
		}
		stopReason(t, c.success(t, id), "end_turn")
	}
	c.configure(t, sid, "approval", "allow_always")
	run("mcp_untrusted_write", "allow_once")
	mode := c.request(t, "session/set_mode", map[string]any{"sessionId": sid, "modeId": "plan"})
	update(t, c.read(t), sid, "current_mode_update")
	c.success(t, mode)
	run("mcp_untrusted_read_hint", "reject_once")
	run("write_file", "reject_once")
	run("read_file", "allow_once")
	mode = c.request(t, "session/set_mode", map[string]any{"sessionId": sid, "modeId": "default"})
	update(t, c.read(t), sid, "current_mode_update")
	c.success(t, mode)
	c.configure(t, sid, "approval", "read_only")
	run("search_files", "allow_once")
	run("mcp_server_read_file", "reject_once")
}
