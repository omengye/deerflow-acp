package mcp

import (
	"context"
	"encoding/base64"
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

	official "github.com/cloudwego/eino-ext/components/tool/mcp/officialmcp"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
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
	if mode == "picture" {
		s := sdk.NewServer(&sdk.Implementation{Name: "image-fixture", Version: "1"}, nil)
		sdk.AddTool(s, &sdk.Tool{Name: "echo.value", Description: "Return a PNG image"}, func(context.Context, *sdk.CallToolRequest, echoArgs) (*sdk.CallToolResult, any, error) {
			data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aZ1sAAAAASUVORK5CYII=")
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "fixture image"}, &sdk.ImageContent{MIMEType: "image/png", Data: data}}}, nil, nil
		})
		_ = s.Run(context.Background(), &sdk.StdioTransport{})
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

func saveDesktopServers(t *testing.T, profile string, servers ...harness.MCPServer) {
	t.Helper()
	dir := filepath.Join(profile, "config")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	entries := []any{}
	for _, server := range servers {
		env := []map[string]string{}
		for k, v := range server.Env {
			env = append(env, map[string]string{"name": k, "value": v})
		}
		entries = append(entries, map[string]any{"name": server.Name, "command": server.Command, "args": server.Args, "env": env})
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client-mcp-servers.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopStdioGrantsFollowSavedDefinition(t *testing.T) {
	profile := t.TempDir()
	dir := filepath.Join(profile, "config")
	original := processConfig(t, "desktop")
	save := func(servers ...harness.MCPServer) { t.Helper(); saveDesktopServers(t, profile, servers...) }
	manager := testManager(t, harness.MCPPolicy{})
	manager.SetDesktopProfile(profile)
	check := func(s harness.MCPServer, want bool) {
		t.Helper()
		_, _, err := manager.prepare(profile, []harness.MCPServer{s})
		if (err == nil) != want || (!want && !errors.Is(err, harness.ErrPermissionDenied)) {
			t.Fatalf("desktop grant %q expected %v: %v", s.Name, want, err)
		}
	}
	check(original, false)
	second := original
	second.Name, second.Args = "another", []string{"second"}
	save(original, second)
	check(original, true)
	check(second, true)
	changed := original
	changed.Args = second.Args
	check(changed, false)
	changed = original
	changed.Env = map[string]string{"different": "value"}
	check(changed, false)
	changed = original
	changed.Command = filepath.Join(dir, "different-program")
	check(changed, false)
	// Equal sizes and timestamps also occur in real Windows package updates.
	program := filepath.Join(dir, "updating.exe")
	install := func() {
		t.Helper()
		if err := os.WriteFile(program, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(499190400, 0)
		if err := os.Chtimes(program, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	install()
	updating := original
	updating.Name, updating.Command = "updating", program
	save(updating, second)
	check(updating, true)
	standalone := testManager(t, harness.MCPPolicy{AllowedCommands: []string{program}})
	if err := os.Rename(program, program+".previous"); err != nil {
		t.Fatal(err)
	}
	check(second, true)
	// Authorization survives the installation gap, even for a cold manager.
	cold := testManager(t, harness.MCPPolicy{})
	cold.SetDesktopProfile(profile)
	if _, _, err := cold.prepare(profile, []harness.MCPServer{second}); err != nil {
		t.Fatal(err)
	}
	install()
	check(updating, true)
	if _, _, err := standalone.prepare(profile, []harness.MCPServer{updating}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("standalone policy accepted replacement executable: %v", err)
	}
	save(second)
	check(original, false)
	check(second, true)
	save()
	check(second, false)
	legacy := testManager(t, harness.MCPPolicy{AllowedCommands: []string{original.Command}})
	legacy.SetDesktopProfile(profile)
	if _, _, err := legacy.prepare(profile, []harness.MCPServer{original}); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopProcessLossPreservesForegroundAndDoesNotReplay(t *testing.T) {
	ctx, profile := context.Background(), testutil.CanonicalTempDir(t)
	one, two := processConfig(t, "one"), processConfig(t, "two")
	two.Name = "other"
	effects := filepath.Join(profile, "effects")
	one.Env["HARNESS_MCP_EFFECTS"] = effects
	saveDesktopServers(t, profile, one, two)
	m := testManager(t, harness.MCPPolicy{})
	m.SetDesktopProfile(profile)
	if err := m.Bind(ctx, "owner", "chat", profile, []harness.MCPServer{one, two}); err != nil {
		t.Fatal(err)
	}
	d, generation, turn, err := m.AcquireForTurn(ctx, "chat")
	if err != nil || turn == nil || len(turn.Tools) != 2 {
		t.Fatalf("initial turn: %v %v", d.Servers, err)
	}
	defer turn.Close()
	pinned, err := m.Acquire(ctx, "chat", d, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	call := func(tl tool.BaseTool, value string) error {
		data, _ := json.Marshal(echoArgs{Value: value})
		_, err := tl.(tool.EnhancedInvokableTool).InvokableRun(ctx, &schema.ToolArgument{Text: string(data)})
		return err
	}
	if err := call(turn.Tools[0], "drop"); err == nil {
		t.Fatal("lost response reported success")
	}
	select {
	case <-pinned.Done:
	case <-time.After(3 * time.Second):
		t.Fatal("dead transport retained its pinned lease")
	}
	if err := call(turn.Tools[1], "ok"); err != nil {
		t.Fatalf("healthy sibling failed in the active turn: %v", err)
	}
	_, renewed, next, err := m.AcquireForTurn(ctx, "chat")
	if err != nil || next == nil || len(next.Tools) != 2 || renewed == generation {
		t.Fatalf("next turn did not recover: %v", err)
	}
	defer next.Close()
	if _, err := m.Acquire(ctx, "chat", d, generation); !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatalf("old generation admitted: %v", err)
	}
	data, err := os.ReadFile(effects)
	if err != nil || string(data) != "effect\n" {
		t.Fatalf("uncertain tool effect repeated: %v", err)
	}
}

func TestDesktopSlowDiscoveryDoesNotHoldChatUntilConnectTimeout(t *testing.T) {
	ctx, profile := context.Background(), testutil.CanonicalTempDir(t)
	slow, healthy := processConfig(t, "slowlist"), processConfig(t, "ok")
	slow.Name, healthy.Name = "slow", "healthy"
	saveDesktopServers(t, profile, slow, healthy)
	m := testManager(t, harness.MCPPolicy{ConnectTimeout: time.Minute})
	m.SetDesktopProfile(profile)
	// This fixture never answers tools/list. The healthy program and first
	// prompt can proceed while its connection is still initializing.
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := m.Bind(bounded, "owner", "chat", profile, []harness.MCPServer{slow, healthy}); err != nil {
		t.Fatal(err)
	}
	d, _, turn, err := m.AcquireForTurn(bounded, "chat")
	if err != nil || turn == nil || strings.Join(d.Servers, ",") != "healthy" {
		t.Fatalf("initial turn: %v %v", d.Servers, err)
	}
	defer turn.Close()
	if err := m.Release(bounded, "owner", "chat"); err != nil {
		t.Fatal(err)
	}
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

func TestRealMCPImageUsesEnhancedResultWithoutBase64Text(t *testing.T) {
	cfg := processConfig(t, "picture")
	cwd := testutil.CanonicalTempDir(t)
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	if err := m.Bind(context.Background(), "owner", "image-session", cwd, []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	plain := invoke(t, firstTool(t, m, "image-session"), "go")
	if strings.Contains(plain, "iVBORw0KGgo") || !strings.Contains(plain, "omitted from JSON text result") {
		t.Fatal("official text result retained inline image data")
	}
	tools, err := m.EnhancedTools(context.Background(), "image-session")
	if err != nil || len(tools) != 1 {
		t.Fatalf("enhanced tools: %d, %v", len(tools), err)
	}
	run := tools[0].(tool.EnhancedInvokableTool)
	result, err := run.InvokableRun(context.Background(), &schema.ToolArgument{Text: `{"value":"go"}`})
	if err != nil || len(result.Parts) != 2 || result.Parts[1].Image == nil || result.Parts[1].Image.Base64Data == nil {
		t.Fatalf("enhanced image missing: %+v %v", result, err)
	}
	if strings.Contains(result.Parts[0].Text, "iVBORw0KGgo") || result.Parts[1].Image.MIMEType != "image/png" {
		t.Fatal("image data leaked into text or MIME changed")
	}
}

func TestMCPBinaryResultGuardRejectsUnsupportedAndOversizedContent(t *testing.T) {
	ctx := context.Background()
	for _, content := range []sdk.Content{
		&sdk.AudioContent{MIMEType: "audio/wav", Data: []byte("secret-audio")},
		&sdk.ImageContent{MIMEType: "image/png", Data: make([]byte, harness.MaxInputImageBytes+1)},
	} {
		if _, err := captureToolImages(ctx, official.ToolCallInfo{}, &sdk.CallToolResult{Content: []sdk.Content{content}}); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("unsafe binary content accepted: %v", err)
		}
	}
}

func TestStdioLifecycleEnvironmentAndStaleTools(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "host-provider-secret-must-not-inherit")
	cfg := processConfig(t, "one")
	cwd := testutil.CanonicalTempDir(t)
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
	cwd := testutil.CanonicalTempDir(t)
	// Spawning the fixture process can exceed 300 ms under a full package run.
	// Keep the stalled candidate bounded while allowing ordinary startup time.
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}, ConnectTimeout: 3 * time.Second})
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
	cwd := testutil.CanonicalTempDir(t)
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
	cwd := testutil.CanonicalTempDir(t)
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
	cwd := testutil.CanonicalTempDir(t)
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
