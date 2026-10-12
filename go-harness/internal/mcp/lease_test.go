package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func describeLease(t *testing.T, m *Manager, id string) (harness.MCPResourceDescriptor, string) {
	t.Helper()
	d, generation, err := m.Describe(context.Background(), id)
	if err != nil || d.Version != 1 || generation == "" {
		t.Fatalf("Describe: %+v %q %v", d, generation, err)
	}
	return d, generation
}

func TestResourceIdentitySurvivesReconnectButDoesNotAuthorizeIt(t *testing.T) {
	ctx := context.Background()
	cfg, cwd := processConfig(t, "one"), t.TempDir()
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	bind := func() {
		t.Helper()
		if err := m.Bind(ctx, "owner", "s", cwd, []harness.MCPServer{cfg}); err != nil {
			t.Fatal(err)
		}
	}
	bind()
	d, generation := describeLease(t, m, "s")
	data, _ := json.Marshal(d)
	for _, forbidden := range []string{fixtureSecret, cfg.Command, cwd, "HARNESS_MCP", "headers", "command"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("descriptor exposed connection data: %s", forbidden)
		}
	}
	lease, err := m.Acquire(ctx, "s", d, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := m.Release(ctx, "owner", "s"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Done:
	case <-time.After(time.Second):
		t.Fatal("release did not invalidate lease")
	}
	if !errors.Is(lease.Check(), ErrClosed) {
		t.Fatal("released lease remained valid")
	}
	bind()
	d2, generation2 := describeLease(t, m, "s")
	if !reflect.DeepEqual(d, d2) || generation == generation2 {
		t.Fatal("stable identity or connection generation is wrong")
	}
	if _, err := m.Acquire(ctx, "s", d, generation); !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatalf("old generation authorized reconnect: %v", err)
	}
	current, err := m.Acquire(ctx, "s", d, generation2)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	cfg.Env["HARNESS_MCP_EXTRA"] = "changed-connection-intent"
	bind()
	d3, generation3 := describeLease(t, m, "s")
	if d3.ConfigDigest == d.ConfigDigest {
		t.Fatal("configuration change not fingerprinted")
	}
	if _, err := m.Acquire(ctx, "s", d, generation3); !errors.Is(err, harness.ErrResourcesIncompatible) {
		t.Fatalf("changed config accepted: %v", err)
	}
}

func TestLeaseCloseIsLocalAndPreservesEnhancedMedia(t *testing.T) {
	ctx := context.Background()
	cfg := processConfig(t, "picture")
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	defer m.Close()
	if err := m.Bind(ctx, "owner", "s", t.TempDir(), []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	d, generation := describeLease(t, m, "s")
	first, err := m.Acquire(ctx, "s", d, generation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Acquire(ctx, "s", d, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Check(); err != nil {
		t.Fatalf("borrower closed sibling: %v", err)
	}
	args := &schema.ToolArgument{Text: `{"value":"go"}`}
	_, err = first.Tools[0].(tool.EnhancedInvokableTool).InvokableRun(ctx, args)
	var noEffect *harness.ToolNoEffectError
	if !errors.As(err, &noEffect) {
		t.Fatalf("closed borrower dispatched: %v", err)
	}
	result, err := second.Tools[0].(tool.EnhancedInvokableTool).InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Parts) != 2 || result.Parts[1].Image == nil || result.Parts[1].Image.Base64Data == nil {
		t.Fatalf("image was lost: %+v", result)
	}
	if strings.Contains(result.Parts[0].Text, "iVBORw0KGgo") {
		t.Fatal("image leaked into text")
	}
	if got, err := m.Generation(ctx, "s"); err != nil || got != generation {
		t.Fatalf("borrower changed parent: %q %v", got, err)
	}
}

func TestAcquireChecksActualSchemaOnSameConnection(t *testing.T) {
	ctx := context.Background()
	server := fixtureServer("one", "original description", nil)
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	defer httpServer.Close()
	m := testManager(t, harness.MCPPolicy{AllowHTTP: true})
	defer m.Close() // Join the streaming client before closing the fixture server.
	cfg := harness.MCPServer{Name: "fixture", Transport: "http", URL: httpServer.URL}
	if err := m.Bind(ctx, "owner", "s", t.TempDir(), []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	d, generation := describeLease(t, m, "s")
	sdk.AddTool(server, &sdk.Tool{Name: "echo.value", Description: "changed tool contract"}, func(context.Context, *sdk.CallToolRequest, echoArgs) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "new"}}}, nil, nil
	})
	if _, err := m.Acquire(ctx, "s", d, generation); !errors.Is(err, harness.ErrResourcesIncompatible) {
		t.Fatalf("schema changed without refusal: %v", err)
	}
}

func TestResourceIdentityIgnoresJSONObjectOrder(t *testing.T) {
	ctx := context.Background()
	server := sdk.NewServer(&sdk.Implementation{Name: "schema-order-fixture", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "record", Description: "Record a structured request"}, func(context.Context, *sdk.CallToolRequest, struct {
		Name      string   `json:"name"`
		Prompt    string   `json:"prompt"`
		AgentType string   `json:"agent_type"`
		Workspace string   `json:"workspace"`
		Enabled   bool     `json:"enabled"`
		Labels    []string `json:"labels"`
		Options   struct {
			Timezone string `json:"timezone"`
			Schedule string `json:"schedule"`
			Action   string `json:"action"`
		} `json:"options"`
	}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	defer httpServer.Close()
	m := testManager(t, harness.MCPPolicy{AllowHTTP: true})
	defer m.Close()
	if err := m.Bind(ctx, "owner", "s", t.TempDir(), []harness.MCPServer{{Name: "fixture", Transport: "http", URL: httpServer.URL}}); err != nil {
		t.Fatal(err)
	}
	descriptor, generation := describeLease(t, m, "s")
	var firstInfo []byte
	for i := 0; i < 32; i++ {
		lease, err := m.Acquire(ctx, "s", descriptor, generation)
		if err != nil {
			t.Fatalf("unchanged multi-property schema rejected on discovery %d: %v", i, err)
		}
		info, infoErr := lease.Tools[0].Info(ctx)
		data, encodeErr := json.Marshal(info)
		_ = lease.Close()
		if infoErr != nil || encodeErr != nil {
			t.Fatalf("tool metadata: %v %v", infoErr, encodeErr)
		}
		if i == 0 {
			firstInfo = data
		} else if string(data) != string(firstInfo) {
			t.Fatal("unchanged schema altered the execution contract")
		}
		current, currentGeneration := describeLease(t, m, "s")
		if !reflect.DeepEqual(current, descriptor) || currentGeneration != generation {
			t.Fatal("object property order changed resource identity")
		}
	}
}

func TestLeaseTransportLossPreservesUncertainEffectWithoutReplay(t *testing.T) {
	ctx := context.Background()
	cfg := processConfig(t, "one")
	cfg.Env["HARNESS_MCP_EFFECTS"] = filepath.Join(t.TempDir(), "effects.txt")
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	defer m.Close()
	if err := m.Bind(ctx, "owner", "s", t.TempDir(), []harness.MCPServer{cfg}); err != nil {
		t.Fatal(err)
	}
	d, generation := describeLease(t, m, "s")
	lease, err := m.Acquire(ctx, "s", d, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	_, err = lease.Tools[0].(tool.EnhancedInvokableTool).InvokableRun(ctx, &schema.ToolArgument{Text: `{"value":"drop"}`})
	var noEffect *harness.ToolNoEffectError
	if err == nil || errors.As(err, &noEffect) {
		t.Fatalf("unknown external effect hidden: %v", err)
	}
	select {
	case <-lease.Done:
	case <-time.After(3 * time.Second):
		t.Fatal("dead transport did not revoke lease")
	}
	if lease.Check() == nil {
		t.Fatal("dead transport passed readiness")
	}
	data, err := os.ReadFile(cfg.Env["HARNESS_MCP_EFFECTS"])
	if err != nil || string(data) != "effect\n" {
		t.Fatalf("external effect count: %q %v", data, err)
	}
	_, err = lease.Tools[0].(tool.EnhancedInvokableTool).InvokableRun(ctx, &schema.ToolArgument{Text: `{"value":"drop"}`})
	if !errors.As(err, &noEffect) {
		t.Fatalf("revoked lease was not refused before dispatch: %v", err)
	}
}

func TestDescribeEmptyResources(t *testing.T) {
	ctx := context.Background()
	m := testManager(t, harness.MCPPolicy{})
	if err := m.Bind(ctx, "owner", "empty", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"empty", "absent"} {
		d, generation, err := m.Describe(ctx, id)
		if err != nil || generation != "" || !reflect.DeepEqual(d, harness.MCPResourceDescriptor{}) {
			t.Fatalf("empty descriptor: %+v %q %v", d, generation, err)
		}
	}
}

func TestLeaseCloseCancelsOnlyItsInflightCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	entered := make(chan struct{}, 1)
	server := fixtureServer("one", "Echo with a cancellable wait", func(ctx context.Context, args echoArgs) (string, error) {
		if args.Value == "block" {
			entered <- struct{}{}
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "one:" + args.Value, nil
	})
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	defer httpServer.Close()
	m := testManager(t, harness.MCPPolicy{AllowHTTP: true})
	defer m.Close()
	if err := m.Bind(ctx, "owner", "s", t.TempDir(), []harness.MCPServer{{Name: "fixture", Transport: "http", URL: httpServer.URL}}); err != nil {
		t.Fatal(err)
	}
	d, generation := describeLease(t, m, "s")
	lease, err := m.Acquire(ctx, "s", d, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	done := make(chan error, 1)
	go func() {
		_, err := lease.Tools[0].(tool.EnhancedInvokableTool).InvokableRun(ctx, &schema.ToolArgument{Text: `{"value":"block"}`})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("call did not reach real server")
	}
	_ = lease.Close()
	select {
	case err = <-done:
		var noEffect *harness.ToolNoEffectError
		if err == nil || errors.As(err, &noEffect) {
			t.Fatalf("dispatched cancellation claimed no effect: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("borrower call did not cancel")
	}
	if got := invoke(t, firstTool(t, m, "s"), "still-alive"); !strings.Contains(got, "still-alive") {
		t.Fatalf("parent connection was closed: %s", got)
	}
}
