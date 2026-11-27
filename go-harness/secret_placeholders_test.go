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

func TestSecretPlaceholderReachesOnlyTheMCPServer(t *testing.T) {
	t.Run("full_access", func(t *testing.T) {
		testMCPSecretPlaceholder(t, harness.ApprovalAllowAlways, harness.PermissionModeDangerous, false)
	})
	t.Run("global_with_tool_approvals_off", func(t *testing.T) {
		testMCPSecretPlaceholder(t, harness.ApprovalGlobal, harness.PermissionModeOff, true)
	})
}

func testMCPSecretPlaceholder(t *testing.T, approval string, permissionMode harness.PermissionMode, wantNamedApproval bool) {
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
	c, err := Open(ctx, Config{DataDir: dataDir, Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", PermissionMode: permissionMode, DisableSubagents: true, MCP: harness.MCPPolicy{AllowHTTP: true}})
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
	if _, err = c.SetConfigOption(ctx, session.ID, "approval", approval); err != nil {
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
	if wantNamedApproval {
		if len(approvals) != 1 || !strings.HasPrefix(approvals[0].ToolName, "mcp_fixture_lookup_") || !strings.Contains(string(approvals[0].Arguments), "{{secret:API_TOKEN}}") || strings.Contains(string(approvals[0].Arguments), token) {
			t.Fatal("expected one approval identifying the named secret by reference")
		}
	} else if len(approvals) != 0 {
		t.Fatalf("Full Access requested %d approvals for stored secrets", len(approvals))
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
		name, tool, arguments string
		trusted               bool
		wantDefault, wantFull bool
	}{
		{"import_config_write", "secret_manage", `{"action":"import_config"}`, true, true, true},
		{"import_config_dry_run", "secret_manage", `{"action":"import_config","dry_run":true}`, true, false, false},
		{"delete", "secret_manage", `{"action":"delete","name":"K"}`, true, true, true},
		{"restore", "secret_manage", `{"action":"restore","name":"K"}`, true, true, true},
		{"set", "secret_manage", `{"action":"set","name":"K","value_sealed":"x"}`, true, false, false},
		{"set_delete_source", "secret_manage", `{"action":"set","name":"K","from_path":"key","delete_source":true}`, true, true, true},
		{"mcp_placeholder", "mcp_ext_call", `{"token":"{{secret:API_KEY}}"}`, false, true, false},
		{"mcp_escaped_placeholder", "mcp_ext_call", `{"token":"\u007b{secret:API_KEY}}"}`, false, true, false},
		{"mcp_placeholder_and_unbound_credential", "mcp_ext_call", `{"token":"{{secret:API_KEY}}","key":"{{credential:key}}"}`, false, true, true},
		{"mcp_plain", "mcp_ext_call", `{"token":"plain"}`, false, false, false},
		{"web_fetch_placeholder", "web_fetch", `{"url":"https://h/?k={{secret:API_KEY:url}}"}`, true, true, false},
		{"write_file_literal_placeholder", "write_file", `{"path":"doc.md","content":"{{secret:API_KEY}}"}`, true, false, false},
		{"execute_with_secrets", "execute", `{"script":"echo hi","secrets":{"A":"B"}}`, true, true, false},
		{"execute_with_secret_out", "execute", `{"script":"echo hi","secret_out":true}`, true, true, true},
		{"execute_read_and_write", "execute", `{"secrets":{"A":"B"},"secret_out":true}`, true, true, true},
		{"execute_read_and_unbound_credential", "execute", `{"secrets":{"A":"B"},"credential":{"service":"https://service.example"}}`, true, true, true},
		{"external_execute_with_secrets", "execute", `{"secrets":{"A":"B"}}`, false, true, true},
		{"bash_with_secrets", "bash", `{"secrets":{"A":"B"}}`, true, true, false},
		{"skill_with_secrets", "skill_run", `{"secrets":{"A":"B"}}`, true, true, false},
		{"pinned_skill_read", "skill_run", `{"secret_plan":{"read":{"TOKEN":"API_TOKEN"}}}`, true, false, false},
		{"pinned_skill_read_and_write", "skill_run", `{"secret_plan":{"read":{"TOKEN":"API_TOKEN"},"write":["REFRESH"]}}`, true, true, true},
		{"external_skill_read", "skill_run", `{"secret_plan":{"read":{"TOKEN":"API_TOKEN"}}}`, false, true, true},
		{"execute_plain", "execute", `{"script":"echo hi"}`, true, false, false},
		{"bash_empty_args", "bash", "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := harness.PermissionRequest{ToolName: tc.tool, Arguments: json.RawMessage(tc.arguments), TrustedBuiltin: tc.trusted}
			if got := harness.ExplicitSecretApproval(request); got != tc.wantDefault {
				t.Errorf("default explicit approval = %t, want %t", got, tc.wantDefault)
			}
			fullAccess := harness.Session{ApprovalMode: harness.ApprovalAllowAlways}
			if got := harness.ExplicitApprovalForSession(fullAccess, request); got != tc.wantFull {
				t.Errorf("Full Access explicit approval = %t, want %t", got, tc.wantFull)
			}
			fullAccess.Mode = "plan"
			if got := harness.ExplicitApprovalForSession(fullAccess, request); got != tc.wantDefault {
				t.Errorf("plan mode explicit approval = %t, want %t", got, tc.wantDefault)
			}
		})
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
