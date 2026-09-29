package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

type resourceBinding struct {
	owner, id, cwd string
	servers        []harness.MCPServer
}
type fakeResources struct {
	mu            sync.Mutex
	bindings      map[string]resourceBinding
	beforeBind    func(context.Context, resourceBinding) error
	beforeRelease func(context.Context, string) error
	ownerReleases []string
	http, sse     bool
}

func (f *fakeResources) Bind(ctx context.Context, owner, id, cwd string, servers []harness.MCPServer) error {
	encoded, _ := json.Marshal(servers)
	var cloned []harness.MCPServer
	_ = json.Unmarshal(encoded, &cloned)
	binding := resourceBinding{owner, id, cwd, cloned}
	f.mu.Lock()
	hook := f.beforeBind
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, binding); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bindings == nil {
		f.bindings = make(map[string]resourceBinding)
	}
	if old, ok := f.bindings[id]; ok && old.owner != owner {
		return harness.ErrAttachedElsewhere
	}
	f.bindings[id] = binding
	return nil
}
func (f *fakeResources) Release(ctx context.Context, owner, id string) error {
	f.mu.Lock()
	binding, ok := f.bindings[id]
	hook := f.beforeRelease
	f.mu.Unlock()
	if !ok {
		return nil
	}
	if binding.owner != owner {
		return harness.ErrNotAttached
	}
	if hook != nil {
		if err := hook(ctx, id); err != nil {
			return err
		}
	}
	f.mu.Lock()
	delete(f.bindings, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeResources) ReleaseOwner(ctx context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownerReleases = append(f.ownerReleases, owner)
	for id, b := range f.bindings {
		if b.owner == owner {
			delete(f.bindings, id)
		}
	}
	return nil
}
func (f *fakeResources) MCPCapabilities() (bool, bool) { return f.http, f.sse }

func stdioServer(t *testing.T, secret string) map[string]any {
	t.Helper()
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"name": "local-tools", "command": command, "args": []any{"--test-fixture-only"}, "env": []any{map[string]string{"name": "SECRET_TOKEN", "value": secret}}}
}

func TestMCPBindingLeaseCancellationAndNoCredentialPersistence(t *testing.T) {
	started := make(chan resourceBinding, 1)
	gate := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(gate) })
	resources := &fakeResources{beforeBind: func(ctx context.Context, b resourceBinding) error {
		started <- b
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	f.service.Resources = resources
	c := connect(t, f.service)
	c.initialize(t)
	const credential = "fixture-sensitive-env-token"
	newID := c.request(t, "session/new", map[string]any{"cwd": f.cwd, "mcpServers": []any{stdioServer(t, credential)}})
	var binding resourceBinding
	select {
	case binding = <-started:
	case <-time.After(time.Second):
		t.Fatal("MCP binding not started")
	}
	if binding.servers[0].Env["SECRET_TOKEN"] != credential || binding.servers[0].Transport != "stdio" {
		t.Fatalf("stdio conversion failed")
	}
	prompt := c.request(t, "session/prompt", promptParams(binding.id, "during bind"))
	if msg := c.response(t, prompt); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("prompt entered resource setup: %+v", msg)
	}
	once.Do(func() { close(gate) })
	result := c.success(t, newID)
	if strings.Contains(string(result), credential) {
		t.Fatal("credentials appeared in session response")
	}
	var session struct {
		ID string `json:"sessionId"`
	}
	_ = json.Unmarshal(result, &session)
	if session.ID != binding.id {
		t.Fatal("bound wrong session")
	}
	// Resource configurations are never written to any business table.
	for _, query := range []string{`SELECT COALESCE(group_concat(event),'') FROM harness_events`, `SELECT COALESCE(group_concat(intent),'') FROM harness_approvals`, `SELECT COALESCE(group_concat(model||title||cwd),'') FROM harness_sessions`} {
		var data string
		if err := f.db.DB().QueryRow(query).Scan(&data); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(data, credential) {
			t.Fatal("MCP credential persisted")
		}
	}
	_ = c.in.Close()
	select {
	case <-c.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("EOF did not clean resources")
	}
	resources.mu.Lock()
	remaining := len(resources.bindings)
	released := len(resources.ownerReleases)
	resources.mu.Unlock()
	if remaining != 0 || released != 1 {
		t.Fatalf("remaining=%d owner releases=%d", remaining, released)
	}
}

func TestMCPFailedReconfigurationPreservesBoundResources(t *testing.T) {
	resources := &fakeResources{beforeBind: func(_ context.Context, b resourceBinding) error {
		if len(b.servers) > 0 && b.servers[0].Env["SECRET_TOKEN"] == "replacement-fails" {
			return fmt.Errorf("%w: server connection rejected", harness.ErrInvalidInput)
		}
		return nil
	}}
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	f.service.Resources = resources
	c := connect(t, f.service)
	c.initialize(t)
	id := c.request(t, "session/new", map[string]any{"cwd": f.cwd, "mcpServers": []any{stdioServer(t, "original")}})
	var result struct {
		ID string `json:"sessionId"`
	}
	_ = json.Unmarshal(c.success(t, id), &result)
	rebind := c.request(t, "session/resume", map[string]any{"cwd": f.cwd, "sessionId": result.ID, "mcpServers": []any{stdioServer(t, "replacement-fails")}})
	if msg := c.response(t, rebind); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("failed replacement=%+v", msg)
	}
	resources.mu.Lock()
	secret := resources.bindings[result.ID].servers[0].Env["SECRET_TOKEN"]
	resources.mu.Unlock()
	if secret != "original" {
		t.Fatal("failed replacement destroyed original binding")
	}
	prompt := c.request(t, "session/prompt", promptParams(result.ID, "still usable"))
	stopReason(t, c.success(t, prompt), "end_turn")
}

func TestResourceCloseCompletesBeforeAnotherConnectionCanAttach(t *testing.T) {
	resources := &fakeResources{}
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	f.service.Resources = resources
	a := connect(t, f.service)
	a.initialize(t)
	sid := a.newSession(t, f.cwd)
	b := connect(t, f.service)
	b.initialize(t)
	other := b.newSession(t, f.cwd)
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(gate) })
	resources.mu.Lock()
	resources.beforeRelease = func(ctx context.Context, id string) error {
		if id == sid {
			started <- struct{}{}
			select {
			case <-gate:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	resources.mu.Unlock()
	closeID := a.request(t, "session/close", map[string]any{"sessionId": sid})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("resource release did not start")
	}
	resume := b.request(t, "session/resume", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	if msg := b.response(t, resume); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("attached before cleanup: %+v", msg)
	}
	prompt := b.request(t, "session/prompt", promptParams(other, "unrelated session"))
	stopReason(t, b.success(t, prompt), "end_turn")
	once.Do(func() { close(gate) })
	a.success(t, closeID)
	resume = b.request(t, "session/resume", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	b.success(t, resume)
	_ = a.in.Close()
	select {
	case <-a.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect incomplete")
	}
	resources.mu.Lock()
	_, stillBound := resources.bindings[sid]
	resources.mu.Unlock()
	if !stillBound {
		t.Fatal("old owner's disconnect released new owner's resource")
	}
}

func TestMCPWireVariantsRejectMalformedConfigurationWithoutLeakingSecrets(t *testing.T) {
	f := newFixture(t, nil)
	resources := &fakeResources{}
	f.service.Resources = resources
	c := connect(t, f.service)
	c.initialize(t)
	command, _ := os.Executable()
	cases := []map[string]any{
		{"name": "bad", "type": "unknown", "command": command, "args": []any{}, "env": []any{}},
		{"name": "bad", "command": command, "args": []any{}, "env": map[string]string{"SECRET": "do-not-echo"}},
		{"name": "bad", "command": command, "args": []any{}, "env": []any{map[string]string{"name": "KEY", "value": "do-not-echo"}, map[string]string{"name": "KEY", "value": "duplicate"}}},
		{"name": "bad", "type": "http", "url": "https://example.invalid", "headers": []any{map[string]string{"name": "Authorization", "value": "do-not-echo"}}},
		{"name": "bad", "command": "relative-command", "args": []any{}, "env": []any{}},
		{"name": "bad", "command": command, "args": []any{nil}, "env": []any{}},
		{"name": "bad", "command": command, "args": []any{}, "env": []any{map[string]any{"name": "KEY"}}},
		{"name": "bad", "type": nil, "command": command, "args": []any{}, "env": []any{}},
	}
	for _, server := range cases {
		id := c.request(t, "session/new", map[string]any{"cwd": f.cwd, "mcpServers": []any{server}})
		msg := c.response(t, id)
		if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("invalid MCP accepted: %+v", msg)
		}
		data, _ := json.Marshal(msg)
		if strings.Contains(string(data), "do-not-echo") {
			t.Fatal("credential leaked in error")
		}
	}
	resources.mu.Lock()
	count := len(resources.bindings)
	resources.mu.Unlock()
	if count != 0 {
		t.Fatal("malformed configuration reached resource binding")
	}
}

func TestMCPHTTPAndSSECapabilityAndHeaderConversion(t *testing.T) {
	resources := &fakeResources{http: true, sse: true}
	f := newFixture(t, nil)
	f.service.Resources = resources
	c := connect(t, f.service)
	id := c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var response struct {
		Caps struct {
			MCP struct {
				HTTP bool `json:"http"`
				SSE  bool `json:"sse"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(c.success(t, id), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Caps.MCP.HTTP || !response.Caps.MCP.SSE {
		t.Fatal("installed MCP capabilities omitted")
	}
	for _, kind := range []string{"http", "sse"} {
		id = c.request(t, "session/new", map[string]any{"cwd": f.cwd, "mcpServers": []any{map[string]any{"name": kind, "type": kind, "url": "https://example.invalid/mcp", "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer fixture"}}}}})
		var created struct {
			ID string `json:"sessionId"`
		}
		_ = json.Unmarshal(c.success(t, id), &created)
		resources.mu.Lock()
		server := resources.bindings[created.ID].servers[0]
		resources.mu.Unlock()
		if server.Transport != kind || server.Headers["Authorization"] != "Bearer fixture" {
			t.Fatal("header conversion failed")
		}
	}
}

func TestRejectedMCPStartupDoesNotLeavePhantomSession(t *testing.T) {
	resources := &fakeResources{beforeBind: func(context.Context, resourceBinding) error {
		return fmt.Errorf("%w: startup policy rejected", harness.ErrInvalidInput)
	}}
	f := newFixture(t, nil)
	f.service.Resources = resources
	c := connect(t, f.service)
	c.initialize(t)
	id := c.request(t, "session/new", map[string]any{"cwd": f.cwd, "mcpServers": []any{stdioServer(t, "private")}})
	if msg := c.response(t, id); msg.Error == nil {
		t.Fatal("startup rejection was ignored")
	}
	listed := c.request(t, "session/list", map[string]any{})
	var response struct {
		Sessions []any `json:"sessions"`
	}
	_ = json.Unmarshal(c.success(t, listed), &response)
	if len(response.Sessions) != 0 {
		t.Fatal("failed session/new left a phantom session")
	}
}
