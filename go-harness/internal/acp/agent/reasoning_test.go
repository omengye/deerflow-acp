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

func TestSessionReasoningACP(t *testing.T) {
	observed := make(chan harness.Session, 1)
	started, finish := make(chan struct{}, 1), make(chan struct{})
	engine := engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		observed <- req.Session
		started <- struct{}{}
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	f := newFixture(t, engine)
	f.service.Settings = hr.ConfigSettings{
		Models:                []harness.ConfigValue{{Value: "fake-model"}, {Value: "other"}},
		ModelReasoningEfforts: map[string][]string{"fake-model": {"low", "high", "max"}},
	}
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	values := c.configure(t, sid, "reasoning_effort", "high")
	if values["model"] != "fake-model" {
		t.Fatal(values)
	}
	bad := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": "reasoning_effort", "value": "unsupported"})
	if msg := c.response(t, bad); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("invalid effort accepted: %+v", msg)
	}
	if values := c.configure(t, sid, "reasoning_effort", "max"); values["reasoning_effort"] != "max" {
		t.Fatalf("max effort was not accepted: %v", values)
	}
	c.configure(t, sid, "reasoning_effort", "high")
	prompt := c.request(t, "session/prompt", promptParams(sid, "inspect"))
	<-started
	if x := <-observed; x.ReasoningEffort != "high" {
		t.Fatalf("engine effort=%q", x.ReasoningEffort)
	}
	busy := c.request(t, "session/set_config_option", map[string]any{"sessionId": sid, "configId": "reasoning_effort", "value": "low"})
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
	raw := next.success(t, resume)
	if configValues(t, raw)["reasoning_effort"] != "high" {
		t.Fatal("effort not restored")
	}
	var response struct {
		Options []harness.ConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	for _, option := range response.Options {
		if option.ID == "reasoning_effort" && (option.Category != "thought_level" || option.Options[0].Value != "default") {
			t.Fatalf("invalid option: %+v", option)
		}
		if option.ID == "model" && option.Meta == nil {
			t.Fatal("missing per-model capability metadata")
		}
	}
	next.configure(t, sid, "reasoning_effort", "default")
	if x, err := store.Session(context.Background(), sid); err != nil || x.ReasoningEffort != "" {
		t.Fatalf("default not cleared: %+v %v", x, err)
	}
	next.configure(t, sid, "reasoning_effort", "high")
	if values := next.configure(t, sid, "model", "other"); values["reasoning_effort"] != "" {
		t.Fatal("unsupported model exposes effort")
	}
	if x, err := store.Session(context.Background(), sid); err != nil || x.ReasoningEffort != "" {
		t.Fatalf("model switch retained effort: %+v %v", x, err)
	}
	if values := next.configure(t, sid, "model", "fake-model"); values["reasoning_effort"] != "default" {
		t.Fatal(values)
	}
}
