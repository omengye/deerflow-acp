package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const fixtureSecret = "mcp-private-fixture-token-29418"

type echoArgs struct {
	Value string `json:"value"`
}

func fixtureServer(label, description string, call func(context.Context, echoArgs) (string, error)) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "echo.value", Description: description}, func(ctx context.Context, _ *sdk.CallToolRequest, args echoArgs) (*sdk.CallToolResult, any, error) {
		value := label + ":" + args.Value
		var err error
		if call != nil {
			value, err = call(ctx, args)
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: value}}}, nil, err
	})
	return s
}

// A real MCP SDK stdio server in a separate executable, not a mocked transport.
func TestMCPProcessHelper(t *testing.T) {
	if os.Getenv("HARNESS_MCP_FIXTURE") != "1" {
		return
	}
	mode := os.Getenv("HARNESS_MCP_MODE")
	if mode == "stall" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	desc := "Echo fixture input"
	if mode == "leakmeta" {
		desc = fixtureSecret
	}
	s := fixtureServer(mode, desc, func(ctx context.Context, args echoArgs) (string, error) {
		switch args.Value {
		case "environment":
			cwd, _ := os.Getwd()
			b, _ := json.Marshal(map[string]any{"providerAbsent": os.Getenv("OPENAI_API_KEY") == "", "explicitPresent": os.Getenv("HARNESS_MCP_TOKEN") == fixtureSecret, "cwdOK": cwd == os.Getenv("HARNESS_MCP_CWD"), "pid": os.Getpid()})
			return string(b), nil
		case "block":
			<-ctx.Done()
			return "", ctx.Err()
		case "leak":
			return fixtureSecret, nil
		case "drop":
			f, err := os.OpenFile(os.Getenv("HARNESS_MCP_EFFECTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(2)
			}
			_, _ = f.WriteString("effect\n")
			_ = f.Close()
			os.Exit(3)
		}
		return mode + ":" + args.Value, nil
	})
	if mode == "slowlist" {
		s.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
			return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
				if method == "tools/list" {
					if ready := os.Getenv("HARNESS_MCP_READY"); ready != "" {
						_ = os.WriteFile(ready, []byte("ready"), 0600)
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return next(ctx, method, req)
			}
		})
	}
	_ = s.Run(context.Background(), &sdk.StdioTransport{})
	os.Exit(0) // Never emit the testing package's PASS line into protocol stdout.
}

func processConfig(t *testing.T, mode string) harness.MCPServer {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return harness.MCPServer{Name: "fixture", Command: exe, Args: []string{"-test.run=^TestMCPProcessHelper$"}, Env: map[string]string{"HARNESS_MCP_FIXTURE": "1", "HARNESS_MCP_MODE": mode, "HARNESS_MCP_TOKEN": fixtureSecret}}
}

func testManager(t *testing.T, policy harness.MCPPolicy) *Manager {
	t.Helper()
	if policy.CloseTimeout == 0 {
		policy.CloseTimeout = time.Second
	}
	if policy.ConnectTimeout == 0 {
		policy.ConnectTimeout = 3 * time.Second
	}
	m, err := New(policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func firstTool(t *testing.T, m *Manager, id string) tool.InvokableTool {
	t.Helper()
	ts, err := m.Tools(context.Background(), id)
	if err != nil || len(ts) != 1 {
		t.Fatalf("Tools = %d, %v", len(ts), err)
	}
	return ts[0].(tool.InvokableTool)
}

func invoke(t *testing.T, tt tool.InvokableTool, value string) string {
	t.Helper()
	args, _ := json.Marshal(echoArgs{Value: value})
	got, err := tt.InvokableRun(context.Background(), string(args))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestStdioLifecycleEnvironmentAndStaleTools(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "host-provider-secret-must-not-inherit")
	cfg := processConfig(t, "one")
	cwd := t.TempDir()
	cfg.Env["HARNESS_MCP_CWD"] = cwd
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Bind(ctx, "owner", "s", cwd, []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	cancel() // Setup request ending must not close the established session.
	tt := firstTool(t, m, "s")
	if got := invoke(t, tt, "hello"); !strings.Contains(got, "one:hello") {
		t.Fatal(got)
	}
	if got := invoke(t, tt, "environment"); !strings.Contains(got, `\"providerAbsent\":true`) || !strings.Contains(got, `\"explicitPresent\":true`) || !strings.Contains(got, `\"cwdOK\":true`) {
		t.Fatal(got)
	}
	info, err := tt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.Name, "mcp_fixture_echo_value_") || len(info.Name) > 64 {
		t.Fatal(info.Name)
	}
	info.Name = "tampered"
	next, _ := tt.Info(context.Background())
	if next.Name == "tampered" {
		t.Fatal("tool schema is caller mutable")
	}
	if _, err := tt.InvokableRun(context.Background(), `{"value":"leak"}`); err == nil || strings.Contains(err.Error(), fixtureSecret) {
		t.Fatalf("credential result: %v", err)
	}
	b := m.bindings["s"]
	proc := b.endpoints[0].transport.conn.(*commandConnection)
	if err := m.Release(context.Background(), "other", "s"); !errors.Is(err, harness.ErrAttachedElsewhere) {
		t.Fatal(err)
	}
	if err := m.Release(context.Background(), "owner", "s"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.exited:
	default:
		t.Fatal("stdio child was not reaped")
	}
	if _, err := tt.InvokableRun(context.Background(), `{"value":"hello"}`); !errors.Is(err, ErrClosed) {
		t.Fatalf("stale tool: %v", err)
	}
	if err := m.ReleaseOwner(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind(context.Background(), "owner", "new", cwd, nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestAtomicReplacementAndFailedCandidate(t *testing.T) {
	cfg := processConfig(t, "one")
	cwd := t.TempDir()
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}, ConnectTimeout: 300 * time.Millisecond})
	if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	old := firstTool(t, m, "s")
	oldBinding := m.bindings["s"]
	if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	if m.bindings["s"] != oldBinding {
		t.Fatal("identical Bind replaced process")
	}
	bad := processConfig(t, "stall")
	if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{bad}); err == nil {
		t.Fatal("stalled setup succeeded")
	}
	if m.bindings["s"] != oldBinding || !strings.Contains(invoke(t, old, "ok"), "one:ok") {
		t.Fatal("failed candidate destroyed active generation")
	}
	if err := m.Bind(context.Background(), "intruder", "s", cwd, []harness.MCPServer{cfg}); !errors.Is(err, harness.ErrAttachedElsewhere) {
		t.Fatal(err)
	}
	newCfg := processConfig(t, "two")
	if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{newCfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.InvokableRun(context.Background(), `{"value":"ok"}`); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if !strings.Contains(invoke(t, firstTool(t, m, "s"), "ok"), "two:ok") {
		t.Fatal("replacement unavailable")
	}
}

func TestStdioCallCancellationAndNoReplay(t *testing.T) {
	cfg := processConfig(t, "one")
	cfg.Env["HARNESS_MCP_EFFECTS"] = filepath.Join(t.TempDir(), "effects.txt")
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}, CallTimeout: 100 * time.Millisecond})
	if err := m.Bind(context.Background(), "owner", "s", t.TempDir(), []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	tt := firstTool(t, m, "s")
	if _, err := tt.InvokableRun(context.Background(), `{"value":"block"}`); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call deadline: %v", err)
	}
	if !strings.Contains(invoke(t, tt, "again"), "one:again") {
		t.Fatal("cancelled call killed session")
	}
	if _, err := tt.InvokableRun(context.Background(), `{"value":"drop"}`); err == nil {
		t.Fatal("connection loss succeeded")
	}
	data, err := os.ReadFile(cfg.Env["HARNESS_MCP_EFFECTS"])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "effect\n" {
		t.Fatalf("side effect replayed: %q", data)
	}
}

func TestPolicyAndCredentialMetadata(t *testing.T) {
	cfg := processConfig(t, "one")
	cwd := t.TempDir()
	m := testManager(t, harness.MCPPolicy{})
	if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{cfg}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal(err)
	}
	if _, err := New(harness.MCPPolicy{AllowedCommands: []string{"relative-executable"}}); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal(err)
	}
	trusted := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	cfg.Env["HARNESS_MCP_MODE"] = "leakmeta"
	if err := trusted.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{cfg}); err == nil || strings.Contains(err.Error(), fixtureSecret) {
		t.Fatalf("metadata leakage: %v", err)
	}
	if len(trusted.all) != 0 {
		t.Fatal("failed setup left live binding")
	}
	for _, transport := range []string{"http", "sse"} {
		netCfg := harness.MCPServer{Name: "net", Transport: transport, URL: "http://127.0.0.1:1"}
		if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{netCfg}); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatal(err)
		}
	}
}

func TestHTTPAndSSETransports(t *testing.T) {
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int32
			var wrongHeaders atomic.Int32
			s := fixtureServer("net", "Echo input", func(ctx context.Context, args echoArgs) (string, error) {
				calls.Add(1)
				return "net:" + args.Value, nil
			})
			var handler http.Handler
			if transport == "http" {
				handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, &sdk.StreamableHTTPOptions{JSONResponse: true})
			} else {
				handler = sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return s }, nil)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+fixtureSecret {
					wrongHeaders.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			m := testManager(t, harness.MCPPolicy{AllowHTTP: true, AllowSSE: true})
			defer m.Close() // Close streaming connections before httptest.Server.Close.
			cfg := harness.MCPServer{Name: "network", Transport: transport, URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer " + fixtureSecret}}
			ctx, cancel := context.WithCancel(context.Background())
			if err := m.Bind(ctx, "owner", "s", t.TempDir(), []harness.MCPServer{cfg}); err != nil {
				cancel()
				t.Fatal(err)
			}
			cancel()
			if !strings.Contains(invoke(t, firstTool(t, m, "s"), "ok"), "net:ok") {
				t.Fatal("network call failed")
			}
			if calls.Load() != 1 || wrongHeaders.Load() != 0 {
				t.Fatalf("calls %d, wrong credentials %d", calls.Load(), wrongHeaders.Load())
			}
		})
	}
}

func TestNetworkDoesNotForwardCredentialsAcrossOrigins(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(http.StatusBadRequest) }))
	defer target.Close()
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if transport == "http" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", target.URL)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			m := testManager(t, harness.MCPPolicy{AllowHTTP: true, AllowSSE: true, ConnectTimeout: 200 * time.Millisecond})
			defer m.Close()
			cfg := harness.MCPServer{Name: "network", Transport: transport, URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer " + fixtureSecret}}
			if err := m.Bind(context.Background(), "owner", "s", t.TempDir(), []harness.MCPServer{cfg}); err == nil || strings.Contains(err.Error(), fixtureSecret) {
				t.Fatalf("malicious endpoint: %v", err)
			}
		})
	}
	if received.Load() != 0 {
		t.Fatalf("cross-origin endpoint received %d requests", received.Load())
	}
}

func TestReleaseOwnerCancelsPendingInitialization(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	m := testManager(t, harness.MCPPolicy{AllowHTTP: true})
	defer m.Close()
	done := make(chan error, 1)
	cwd := t.TempDir()
	go func() {
		done <- m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{{Name: "slow", Transport: "http", URL: srv.URL}})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("setup did not start")
	}
	if err := m.ReleaseOwner(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending Bind escaped disconnect")
	}
	if err := m.Bind(context.Background(), "owner", "s", cwd, nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestGlobalToolLimitIsCheckedBeforePublication(t *testing.T) {
	s := fixtureServer("net", "Echo input", nil)
	srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, &sdk.StreamableHTTPOptions{JSONResponse: true}))
	defer srv.Close()
	m := testManager(t, harness.MCPPolicy{AllowHTTP: true, MaxTools: 1})
	defer m.Close()
	configs := []harness.MCPServer{{Name: "one", Transport: "http", URL: srv.URL}, {Name: "two", Transport: "http", URL: srv.URL}}
	if err := m.Bind(context.Background(), "owner", "s", t.TempDir(), configs); err == nil {
		t.Fatal("aggregate tool limit accepted")
	}
	if len(m.bindings) != 0 || len(m.all) != 0 {
		t.Fatal("failed discovery published/leaked resources")
	}
}

func TestStdioSetupDeadlineIncludesToolDiscovery(t *testing.T) {
	cfg := processConfig(t, "slowlist")
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}, ConnectTimeout: 250 * time.Millisecond, CallTimeout: 10 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	err := m.Bind(ctx, "owner", "s", t.TempDir(), []harness.MCPServer{cfg})
	if err == nil {
		t.Fatal("blocked discovery succeeded")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("discovery ignored setup deadline: %v", err)
	}
	if len(m.all) != 0 {
		t.Fatal("failed discovery process not closed")
	}
}

type blockedConnection struct {
	sdk.Connection
	done chan struct{}
}

func (c *blockedConnection) Close() error { <-c.done; return nil }

func TestCleanupTimeoutRetainedUntilManagerClose(t *testing.T) {
	m := testManager(t, harness.MCPPolicy{CloseTimeout: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	b := &binding{owner: "owner", id: "s", ctx: ctx, cancel: cancel, ready: make(chan struct{}), closed: make(chan struct{})}
	conn := &blockedConnection{done: make(chan struct{})}
	defer func() {
		select {
		case <-conn.done:
		default:
			close(conn.done)
		}
	}()
	e := &endpoint{binding: b, ctx: ctx, cancel: cancel, policy: m.policy, closed: make(chan struct{}), transport: &trackedTransport{conn: conn}}
	b.endpoints = []*endpoint{e}
	close(b.ready)
	m.bindings[b.id] = b
	m.all[b] = struct{}{}
	if err := m.Release(context.Background(), "owner", "s"); err == nil {
		t.Fatal("blocked cleanup did not report timeout")
	}
	m.mu.Lock()
	_, tracked := m.all[b]
	m.mu.Unlock()
	if !tracked {
		t.Fatal("timed-out cleanup lost tracking")
	}
	finished := make(chan error, 1)
	go func() { finished <- m.Close() }()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer waitCancel()
	if err := m.WaitClosed(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("global close claimed terminal cleanup: %v", err)
	}
	select {
	case <-m.Done():
		t.Fatal("Done closed while owned cleanup is running")
	default:
	}
	select {
	case err := <-finished:
		t.Fatalf("Close returned before owned cleanup: %v", err)
	default:
	}
	close(conn.done)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join completed cleanup")
	}
	if err := m.WaitClosed(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	n := len(m.all)
	m.mu.Unlock()
	if n != 0 {
		t.Fatal("completed cleanup remains tracked")
	}
}

func TestFailedProcessTerminationRetainsOwnershipUntilExit(t *testing.T) {
	m, err := New(harness.MCPPolicy{CloseTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	defer func() {
		select {
		case <-exited:
		default:
			close(exited)
		}
		_ = m.Close()
	}()
	alreadyClosed := make(chan struct{})
	close(alreadyClosed)
	attempted := make(chan struct{})
	conn := &commandConnection{
		Connection: &blockedConnection{done: alreadyClosed},
		terminate:  func() error { close(attempted); return errors.New("fixture access denied") },
		grace:      time.Millisecond, exited: exited,
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &binding{owner: "owner", id: "s", ctx: ctx, cancel: cancel, ready: make(chan struct{}), closed: make(chan struct{})}
	e := &endpoint{binding: b, ctx: ctx, cancel: cancel, policy: m.policy, closed: make(chan struct{}), transport: &trackedTransport{conn: conn}}
	b.endpoints = []*endpoint{e}
	close(b.ready)
	m.bindings[b.id] = b
	m.all[b] = struct{}{}
	finished := make(chan error, 1)
	go func() { finished <- m.Close() }()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("termination was not attempted")
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer waitCancel()
	if err := m.WaitClosed(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed termination released ownership: %v", err)
	}
	select {
	case <-m.Done():
		t.Fatal("live child marked closed")
	default:
	}
	close(exited) // External intervention/natural exit finally lets Wait reap it.
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("termination failure not reported")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not observe child exit")
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Done not closed after child exit")
	}
}

func TestReleaseCancelsAtomicReplacementBeforePublication(t *testing.T) {
	cfg := processConfig(t, "one")
	cwd := t.TempDir()
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	old := firstTool(t, m, "s")
	candidate := processConfig(t, "slowlist")
	ready := filepath.Join(t.TempDir(), "ready")
	candidate.Env["HARNESS_MCP_READY"] = ready
	done := make(chan error, 1)
	go func() { done <- m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{candidate}) }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waitReady:
	for {
		select {
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				break waitReady
			}
		case <-deadline.C:
			t.Fatal("replacement discovery did not begin")
		}
	}
	if !strings.Contains(invoke(t, old, "active"), "one:active") {
		t.Fatal("old generation unavailable during prepare")
	}
	if err := m.Release(context.Background(), "owner", "s"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement not cancelled")
	}
	if _, err := old.InvokableRun(context.Background(), `{"value":"stale"}`); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := m.Bind(context.Background(), "owner", "s", cwd, []harness.MCPServer{cfg}); err != nil {
		t.Fatalf("Release unexpectedly retired owner: %v", err)
	}
}
