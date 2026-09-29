package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The client passes a real process configuration through ACP. No manager or
// model is mocked inside the harness executable.
func TestACPClientMCPFixture(t *testing.T) {
	if os.Getenv("ACP_MCP_FIXTURE") != "1" {
		return
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "ACP MCP fixture", Version: "1"}, nil)
	if os.Getenv("ACP_MCP_IMAGE") == "1" {
		sdk.AddTool(server, &sdk.Tool{Name: "picture", Description: "Return a PNG picture"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aZ1sAAAAASUVORK5CYII=")
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "picture"}, &sdk.ImageContent{MIMEType: "image/png", Data: data}}}, nil, nil
		})
		_ = server.Run(context.Background(), &sdk.StdioTransport{})
		os.Exit(0)
	}
	sdk.AddTool(server, &sdk.Tool{Name: "record", Description: "Record the requested value"}, func(_ context.Context, _ *sdk.CallToolRequest, args struct {
		Value string `json:"value"`
	}) (*sdk.CallToolResult, any, error) {
		if os.Getenv("ACP_MCP_SECRET") == "" {
			return nil, nil, fmt.Errorf("fixture credentials missing")
		}
		if err := os.WriteFile(os.Getenv("ACP_MCP_EFFECT"), []byte(args.Value), 0600); err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "recorded successfully"}}}, nil, nil
	})
	_ = server.Run(context.Background(), &sdk.StdioTransport{})
	os.Exit(0)
}

func TestACPExecutableClientMCPPermissionConfigAndRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "deerflow-acp-go")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-p=2", "-o", bin, "./cmd/deerflow-acp-go")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	fixture, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	var names [][]string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Model string `json:"model"`
		}
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
			return
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		var toolNames []string
		var remote string
		for _, tt := range body.Tools {
			toolNames = append(toolNames, tt.Function.Name)
			if strings.HasPrefix(tt.Function.Name, "mcp_") {
				remote = tt.Function.Name
			}
		}
		mu.Lock()
		calls = append(calls, string(raw))
		names = append(names, toolNames)
		call := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			if remote == "" {
				t.Error("client MCP tool missing from actual model request")
			}
			chunk := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "remote-1", "type": "function", "function": map[string]any{"name": remote, "arguments": `{"value":"effect after permission"}`}}}}, "finish_reason": nil}}}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	state, cwd := t.TempDir(), t.TempDir()
	effect := filepath.Join(cwd, "remote-effect.txt")
	const secret = "client-mcp-private-credential-583193"
	p := launch(t, bin, state, provider.URL+"/v1", "--mcp-allow-command", fixture, "--allow-model", "alternate-fixture")
	var approvals int
	p.permission = func(raw json.RawMessage) string {
		approvals++
		if _, err := os.Stat(effect); !os.IsNotExist(err) {
			t.Fatalf("remote effect before approval: %v", err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("credentials entered approval")
		}
		return "allow_once"
	}
	p.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	servers := []any{map[string]any{"name": "fixture", "command": fixture, "args": []string{"-test.run=^TestACPClientMCPFixture$"}, "env": []any{map[string]string{"name": "ACP_MCP_FIXTURE", "value": "1"}, map[string]string{"name": "ACP_MCP_EFFECT", "value": effect}, map[string]string{"name": "ACP_MCP_SECRET", "value": secret}}}}
	created := p.request(t, "session/new", map[string]any{"cwd": cwd, "mcpServers": servers})
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(created, &session); err != nil || session.ID == "" {
		t.Fatalf("new=%s", created)
	}
	prompt := map[string]any{"sessionId": session.ID, "prompt": []any{map[string]string{"type": "text", "text": "record the requested value"}}}
	p.request(t, "session/prompt", prompt)
	data, err := os.ReadFile(effect)
	if err != nil || string(data) != "effect after permission" || approvals != 1 {
		t.Fatalf("data=%s approvals=%d err=%v", data, approvals, err)
	}
	p.request(t, "session/set_config_option", map[string]any{"sessionId": session.ID, "configId": "model", "value": "alternate-fixture"})
	p.request(t, "session/set_config_option", map[string]any{"sessionId": session.ID, "configId": "approval", "value": "read_only"})
	p.request(t, "session/prompt", prompt)
	p.request(t, "session/close", map[string]any{"sessionId": session.ID})
	p.request(t, "session/load", map[string]any{"sessionId": session.ID, "cwd": cwd, "mcpServers": []any{}})
	p.request(t, "session/set_config_option", map[string]any{"sessionId": session.ID, "configId": "approval", "value": "ask"})
	p.request(t, "session/prompt", prompt)
	p.stop(t)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("calls=%d", len(calls))
	}
	for _, list := range names[2:] {
		for _, name := range list {
			if strings.HasPrefix(name, "mcp_") {
				t.Fatalf("MCP tool survived readonly or release: %s", name)
			}
		}
	}
	if !strings.Contains(calls[2], `"model":"alternate-fixture"`) {
		t.Fatalf("model config not honored: %s", calls[2])
	}
	for _, raw := range p.updates {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("credentials entered ACP updates")
		}
	}
	for _, raw := range calls {
		if strings.Contains(raw, secret) {
			t.Fatal("credentials entered model context")
		}
	}
	if strings.Contains(p.diagnostics.String(), secret) {
		t.Fatal("credentials entered stderr")
	}
	if err := filepath.WalkDir(state, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("credentials persisted in %s", d.Name())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestACPExecutableMCPImageImportedBeforeModelHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "deerflow-acp-go")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-p=2", "-o", bin, "./cmd/deerflow-acp-go")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	fixture, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const picture = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aZ1sAAAAASUVORK5CYII="
	var calls atomic.Int32
	var hydrated atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			name := ""
			for _, item := range request.Tools {
				if strings.HasPrefix(item.Function.Name, "mcp_") {
					name = item.Function.Name
				}
			}
			if name == "" {
				t.Error("MCP picture tool not exposed to model")
			}
			chunk := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "mcp-picture", "type": "function", "function": map[string]any{"name": name, "arguments": `{}`}}}}, "finish_reason": "tool_calls"}}}
			encoded, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
		} else {
			hydrated.Store(bytes.Contains(body, []byte(picture)))
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Picture received\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	state, cwd := t.TempDir(), t.TempDir()
	p := launch(t, bin, state, provider.URL+"/v1", "--mcp-allow-command", fixture, "--vision-model", "fixture-model", "--disable-subagents")
	p.permission = func(json.RawMessage) string { return "allow_once" }
	p.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	servers := []any{map[string]any{"name": "fixture", "command": fixture, "args": []string{"-test.run=^TestACPClientMCPFixture$"}, "env": []any{map[string]string{"name": "ACP_MCP_FIXTURE", "value": "1"}, map[string]string{"name": "ACP_MCP_IMAGE", "value": "1"}}}}
	created := p.request(t, "session/new", map[string]any{"cwd": cwd, "mcpServers": servers})
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(created, &session); err != nil || session.ID == "" {
		t.Fatalf("session: %s %v", created, err)
	}
	p.request(t, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]string{"type": "text", "text": "Show the MCP picture"}}})
	p.stop(t)
	if calls.Load() != 2 || !hydrated.Load() {
		t.Fatalf("MCP image did not reach model as hydrated content: calls=%d hydrated=%v", calls.Load(), hydrated.Load())
	}
	if err := filepath.WalkDir(state, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(picture)) {
			return fmt.Errorf("inline MCP image persisted in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
