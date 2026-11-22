package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	acpclient "github.com/omengye/deerflow-acp/go-harness/internal/acp/client"
	"github.com/omengye/deerflow-acp/go-harness/internal/opencli"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

func TestWebFetchUsesScopedCredentialWithoutExtraApproval(t *testing.T) {
	token := randomTestSecret(t, "web-token-")
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("web_fetch did not use the bound credential")
		}
		calls.Add(1)
		_, _ = io.WriteString(w, "verified")
	}))
	defer target.Close()
	step := secretToolStep{"web_fetch", map[string]any{"url": target.URL, "credential": "bearer"}}
	provider, seen := secretProvider(t, []secretToolStep{step, step})
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true, PermissionMode: harness.PermissionModeDangerous, BuiltinTools: []harness.BuiltinToolConfig{{Name: "web_fetch"}}}
	var forms atomic.Int32
	ctx := secrets.WithCredentialPrompt(t.Context(), func(_ context.Context, s secrets.CredentialScope, fields secrets.CredentialFields) (secrets.CredentialPair, error) {
		if s.Service != target.URL || s.Consumer != "tool:web_fetch" || fields != secrets.KeyField {
			return secrets.CredentialPair{}, errors.New("unexpected enrollment scope or fields")
		}
		forms.Add(1)
		return secrets.CredentialPair{Key: []byte(token)}, nil
	})
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, err := c.NewSession(ctx, testutil.CanonicalTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetConfigOption(ctx, sess.ID, "approval", harness.ApprovalAllowAlways); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: "Fetch the test service twice."}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		t.Error("web_fetch requested an extra approval")
		return harness.RejectOnce, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || forms.Load() != 1 {
		t.Fatalf("requests=%d enrollment forms=%d", calls.Load(), forms.Load())
	}
	bodies, _ := seen()
	for _, body := range bodies {
		if bytes.Contains(body, []byte(token)) {
			t.Fatal("web credential reached the model")
		}
	}
}

// The same configured MCP tool consumes an unbound named secret and a scoped
// credential. Only the named secret requires per-use disclosure approval.
func TestSecretPlaceholderReachesOnlyTheMCPServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	token := randomTestSecret(t, "mcp-")
	scopedToken := randomTestSecret(t, "mcp-bound-")

	var mu sync.Mutex
	var received []string
	remote := sdk.NewServer(&sdk.Implementation{Name: "lookup-fixture", Version: "1"}, nil)
	sdk.AddTool(remote, &sdk.Tool{Name: "lookup", Description: "Look up an account with an API token"}, func(_ context.Context, _ *sdk.CallToolRequest, in struct {
		Token string `json:"token"`
		Note  string `json:"note"`
	}) (*sdk.CallToolResult, any, error) {
		mu.Lock()
		received = append(received, in.Token)
		mu.Unlock()
		text := "denied"
		if in.Token == token || in.Token == scopedToken {
			text = "authorized as " + in.Token
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil, nil
	})
	mcpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return remote }, &sdk.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(mcpServer.Close)

	var bodies [][]byte
	var requests []secretProviderRequest
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		var req secretProviderRequest
		if err != nil || json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		requests = append(requests, req)
		mu.Unlock()
		name := ""
		for _, item := range req.Tools {
			if strings.HasPrefix(item.Function.Name, "mcp_fixture_lookup_") {
				name = item.Function.Name
			}
		}
		step := len(req.toolResults())
		if name == "" || step >= 3 {
			skillToolResponse(w, req.Stream, "", "", "Looked it up.")
			return
		}
		if step == 0 {
			skillToolResponse(w, req.Stream, name, `{"token":"{{secret:API_TOKEN}}","note":"lookup"}`, "", "call-lookup")
			return
		}
		id := "call-enroll"
		if step == 2 {
			id = "call-reuse"
		}
		skillToolResponse(w, req.Stream, name, `{"token":"{{credential:key}}","note":"lookup"}`, "", id)
	}))
	t.Cleanup(provider.Close)

	dataDir := t.TempDir()
	c, err := Open(ctx, Config{DataDir: dataDir, Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeDangerous, DisableSubagents: true, MCP: harness.MCPPolicy{AllowHTTP: true}})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			c.Close()
		}
	})
	if _, err := c.secrets.Set(ctx, secrets.SetRequest{Name: "API_TOKEN", Kind: secrets.KindString, Value: []byte(token), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	session, err := c.NewSession(ctx, testutil.CanonicalTempDir(t), harness.MCPServer{Name: "fixture", Transport: "http", URL: mcpServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalAllowAlways); err != nil {
		t.Fatal(err)
	}
	var forms atomic.Int32
	ctx = secrets.WithCredentialPrompt(ctx, func(_ context.Context, s secrets.CredentialScope, fields secrets.CredentialFields) (secrets.CredentialPair, error) {
		if s.Service != mcpServer.URL || !strings.HasPrefix(s.Consumer, "tool:mcp_fixture_lookup_") || fields != secrets.KeyField {
			return secrets.CredentialPair{}, errors.New("unexpected MCP credential scope or fields")
		}
		forms.Add(1)
		return secrets.CredentialPair{Key: []byte(scopedToken)}, nil
	})
	var events []harness.RunEvent
	var approvals []harness.PermissionRequest
	if result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Look up my account."}}, func(_ context.Context, event harness.RunEvent) error {
		events = append(events, event)
		return nil
	}, func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals = append(approvals, request)
		return harness.AllowOnce, nil
	}); err != nil {
		t.Fatal(result, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 || received[0] != token || received[1] != scopedToken || received[2] != scopedToken || forms.Load() != 1 {
		t.Fatalf("MCP credential resolution failed: calls=%d forms=%d", len(received), forms.Load())
	}
	var instructed bool
	for i, body := range bodies {
		if bytes.Contains(body, []byte(token)) || bytes.Contains(body, []byte(scopedToken)) {
			t.Fatalf("provider request %d contains the value", i)
		}
		instructed = instructed || bytes.Contains(body, []byte("write {{secret:NAME}} where the value belongs"))
	}
	if !instructed {
		t.Fatal("the model was not told about placeholders")
	}
	if got := latestToolResults(requests)["call-lookup"]; !strings.Contains(got, "authorized as [secret:API_TOKEN]") {
		t.Fatalf("model saw %q", got)
	}
	if len(approvals) != 1 || !strings.HasPrefix(approvals[0].ToolName, "mcp_fixture_lookup_") || !strings.Contains(string(approvals[0].Arguments), "{{secret:API_TOKEN}}") || strings.Contains(string(approvals[0].Arguments), token) {
		t.Fatalf("approvals = %+v", approvals)
	}
	for _, event := range events {
		if data, _ := json.Marshal(event); bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte(scopedToken)) {
			t.Fatalf("%s event contains the value", event.Kind)
		}
	}
	closed = true
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoSecretBytes(t, dataDir, token, scopedToken)
}

func TestExplicitSecretApprovalGuardsHighRiskSecretOperations(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  harness.PermissionRequest
		want bool
	}{
		{"import_config_write", harness.PermissionRequest{ToolName: "secret_manage", TrustedBuiltin: true, Arguments: json.RawMessage(`{"action":"import_config"}`)}, true},
		{"import_config_dry_run", harness.PermissionRequest{ToolName: "secret_manage", TrustedBuiltin: true, Arguments: json.RawMessage(`{"action":"import_config","dry_run":true}`)}, false},
		{"delete", harness.PermissionRequest{ToolName: "secret_manage", TrustedBuiltin: true, Arguments: json.RawMessage(`{"action":"delete","name":"K"}`)}, true},
		{"restore", harness.PermissionRequest{ToolName: "secret_manage", TrustedBuiltin: true, Arguments: json.RawMessage(`{"action":"restore","name":"K"}`)}, true},
		{"set", harness.PermissionRequest{ToolName: "secret_manage", TrustedBuiltin: true, Arguments: json.RawMessage(`{"action":"set","name":"K","value_sealed":"x"}`)}, false},
		{"mcp_placeholder", harness.PermissionRequest{ToolName: "mcp_ext_call", TrustedBuiltin: false, Arguments: json.RawMessage(`{"token":"{{secret:API_KEY}}"}`)}, true},
		{"mcp_escaped_placeholder", harness.PermissionRequest{ToolName: "mcp_ext_call", TrustedBuiltin: false, Arguments: json.RawMessage(`{"token":"\u007b{secret:API_KEY}}"}`)}, true},
		{"mcp_plain", harness.PermissionRequest{ToolName: "mcp_ext_call", TrustedBuiltin: false, Arguments: json.RawMessage(`{"token":"plain"}`)}, false},
		{"web_fetch_placeholder", harness.PermissionRequest{ToolName: "web_fetch", TrustedBuiltin: true, Arguments: json.RawMessage(`{"url":"https://h/?k={{secret:API_KEY:url}}"}`)}, true},
		{"write_file_literal_placeholder", harness.PermissionRequest{ToolName: "write_file", TrustedBuiltin: true, Arguments: json.RawMessage(`{"path":"doc.md","content":"{{secret:API_KEY}}"}`)}, false},
		{"execute_with_secrets", harness.PermissionRequest{ToolName: "execute", TrustedBuiltin: true, Arguments: json.RawMessage(`{"script":"echo hi","secrets":{"A":"B"}}`)}, true},
		{"execute_with_secret_out", harness.PermissionRequest{ToolName: "execute", TrustedBuiltin: true, Arguments: json.RawMessage(`{"script":"echo hi","secret_out":true}`)}, true},
		{"pinned_skill_read", harness.PermissionRequest{ToolName: "skill_run", TrustedBuiltin: true, Arguments: json.RawMessage(`{"secret_plan":{"read":{"TOKEN":"API_TOKEN"}}}`)}, false},
		{"pinned_skill_read_and_write", harness.PermissionRequest{ToolName: "skill_run", TrustedBuiltin: true, Arguments: json.RawMessage(`{"secret_plan":{"read":{"TOKEN":"API_TOKEN"},"write":["REFRESH"]}}`)}, true},
		{"external_skill_read", harness.PermissionRequest{ToolName: "skill_run", Arguments: json.RawMessage(`{"secret_plan":{"read":{"TOKEN":"API_TOKEN"}}}`)}, true},
		{"execute_plain", harness.PermissionRequest{ToolName: "execute", TrustedBuiltin: true, Arguments: json.RawMessage(`{"script":"echo hi"}`)}, false},
		{"bash_empty_args", harness.PermissionRequest{ToolName: "bash", TrustedBuiltin: true}, false},
	} {
		if got := harness.ExplicitSecretApproval(tc.req); got != tc.want {
			t.Errorf("%s: ExplicitSecretApproval = %t; want %t", tc.name, got, tc.want)
		}
	}
}

// Every real host-constructed built-in tool must carry toolidentity.Native so
// middleware never misclassifies it as an external MCP tool for placeholders.
func TestRealBuiltinToolsCarryNativeIdentity(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := testutil.CanonicalTempDir(t)
	backend, err := sandbox.New(context.Background(), harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	cmd, err := tools.CommandTool(backend, harness.SandboxLocal)
	if err != nil {
		t.Fatal(err)
	}
	srun, err := newSkillRunTool(nil, backend, harness.SandboxConfig{}, harness.RunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ocli, err := tools.OpenCLITool(backend, opencli.Launcher{Executable: exe}, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	uocli, err := tools.UnavailableOpenCLITool(errors.New("unavailable"), []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	pres, err := tools.PresentFilesTool(func(context.Context, string, []string) ([]harness.Content, error) { return nil, nil }, 1024)
	if err != nil {
		t.Fatal(err)
	}
	acp, err := acpclient.Tool(t.TempDir(), harness.Session{ID: "s1"}, map[string]harness.ACPAgentConfig{"ext": {Command: exe}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		tool any
	}{
		{"execute", cmd},
		{"skill_run", srun},
		{"host_opencli", ocli},
		{"host_opencli", uocli},
		{"present_files", pres},
		{"invoke_acp_agent", acp},
	} {
		if !toolidentity.IsBuiltin(tc.tool, tc.name) {
			t.Errorf("real built-in tool %q lacks toolidentity.Native", tc.name)
		}
	}
}

func TestAllowAlwaysSessionStillPromptsForExplicitSecretOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	token := randomTestSecret(t, "allow-always-")

	var mu sync.Mutex
	step := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		var req secretProviderRequest
		if err != nil || json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		current := step
		step++
		mu.Unlock()
		switch current {
		case 0:
			skillToolResponse(w, req.Stream, "secret_manage", `{"action":"delete","name":"GUARDED_TOKEN"}`, "", "call-del")
		case 1:
			skillToolResponse(w, req.Stream, "secret_manage", `{"action":"restore","name":"GUARDED_TOKEN"}`, "", "call-rest")
		default:
			skillToolResponse(w, req.Stream, "", "", "Done.")
		}
	}))
	t.Cleanup(provider.Close)

	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeOff, DisableSubagents: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.secrets.Set(ctx, secrets.SetRequest{Name: "GUARDED_TOKEN", Kind: secrets.KindString, Value: []byte(token), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	session, err := c.NewSession(ctx, testutil.CanonicalTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalAllowAlways); err != nil {
		t.Fatal(err)
	}
	var approvals []harness.PermissionRequest
	if result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Delete and restore GUARDED_TOKEN"}}, nil, func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals = append(approvals, request)
		if len(approvals) == 1 {
			return harness.AllowAlways, nil
		}
		return harness.RejectOnce, nil
	}); err != nil {
		t.Fatal(result, err)
	}
	if len(approvals) != 2 {
		t.Fatalf("expected 2 explicit approval prompts despite ApprovalAllowAlways + AllowAlways decision, got %d: %+v", len(approvals), approvals)
	}
	if meta, err := c.secrets.Get(ctx, "GUARDED_TOKEN", false); err != nil || meta.DeletedAt == nil {
		t.Fatalf("GUARDED_TOKEN should remain soft-deleted after rejected restore, got meta=%+v err=%v", meta, err)
	}
}
