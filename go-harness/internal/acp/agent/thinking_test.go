package agent_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSessionThinkingACP(t *testing.T) {
	observed := make(chan harness.Session, 1)
	finish := make(chan struct{})
	engine := engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		observed <- req.Session
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	f := newFixture(t, engine)
	f.service.Settings = hr.ConfigSettings{
		Models:                []harness.ConfigValue{{Value: "fake-model"}, {Value: "other"}},
		ModelReasoningEfforts: map[string][]string{"fake-model": {"low", "high"}},
		ModelThinking:         map[string]harness.ThinkingConfig{"fake-model": {Parameter: "enable_thinking"}},
	}
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	c.configure(t, sid, "reasoning_effort", "high")
	for _, mode := range []string{"on", "off", "on", "default", "off"} {
		values := c.configure(t, sid, "thinking_enabled", mode)
		if values["reasoning_effort"] != "high" {
			t.Fatal("thinking changed effort", values)
		}
	}
	bad := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": "thinking_enabled", "value": "false"})
	if msg := c.response(t, bad); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("accepted invalid mode: %+v", msg)
	}
	prompt := c.request(t, "session/prompt", promptParams(sid, "inspect"))
	if x := <-observed; x.ThinkingMode != "off" || x.ReasoningEffort != "high" {
		t.Fatalf("engine session=%+v", x)
	}
	busy := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": "thinking_enabled", "value": "on"})
	if msg := c.response(t, busy); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("busy setting=%+v", msg)
	}
	close(finish)
	c.success(t, prompt)
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
	values := configValues(t, next.success(t, resume))
	if values["thinking_enabled"] != "off" || values["reasoning_effort"] != "high" {
		t.Fatal("settings not restored", values)
	}
	next.configure(t, sid, "thinking_enabled", "on")
	values = next.configure(t, sid, "model", "other")
	if _, ok := values["thinking_enabled"]; ok {
		t.Fatal("unsupported model advertises switch")
	}
	bad = next.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": "thinking_enabled", "value": "off"})
	if msg := next.response(t, bad); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatal("unsupported model accepted switch")
	}
	values = next.configure(t, sid, "model", "fake-model")
	if values["thinking_enabled"] != "default" {
		t.Fatal("model change retained stale mode", values)
	}
}

func TestSessionThinkingSharedEffortRequiresExplicitLevel(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	f.service.Settings = hr.ConfigSettings{
		Models:                []harness.ConfigValue{{Value: "fake-model"}, {Value: "fixed"}, {Value: "unknown"}},
		ModelReasoningEfforts: map[string][]string{"fake-model": {"low", "high"}},
		ModelThinking:         map[string]harness.ThinkingConfig{"fake-model": {Parameter: "reasoning_effort"}, "fixed": {Parameter: "unsupported"}},
	}
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	reject := func(key, value string) {
		t.Helper()
		id := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": key, "value": value})
		if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("accepted ambiguous selection: %+v", msg)
		}
	}
	reject("thinking_enabled", "on")
	c.configure(t, sid, "reasoning_effort", "high")
	c.configure(t, sid, "thinking_enabled", "on")
	reject("reasoning_effort", "default")
	values := c.configure(t, sid, "thinking_enabled", "off")
	if values["reasoning_effort"] != "high" {
		t.Fatal("off discarded effort")
	}
	values = c.configure(t, sid, "reasoning_effort", "low")
	if values["thinking_enabled"] != "off" {
		t.Fatal("effort turned thinking back on")
	}
	c.configure(t, sid, "thinking_enabled", "on")
	c.configure(t, sid, "thinking_enabled", "default")
	c.configure(t, sid, "reasoning_effort", "default")
	for _, model := range []string{"fixed", "unknown"} {
		values = c.configure(t, sid, "model", model)
		if _, ok := values["thinking_enabled"]; ok {
			t.Fatal("unsupported switch advertised")
		}
		reject("thinking_enabled", "off")
	}
}
