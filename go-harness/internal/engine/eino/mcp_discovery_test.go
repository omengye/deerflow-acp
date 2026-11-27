package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
)

func TestMCPModelDefinitionsAndOriginalNameDispatch(t *testing.T) {
	for _, n := range []int{0, 10, 11, 128} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ctx := context.Background()
			server := sdk.NewServer(&sdk.Implementation{Name: "catalog", Version: "1"}, &sdk.ServerOptions{Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{ListChanged: true}}})
			var effects atomic.Int32
			for j := range n {
				sdk.AddTool(server, &sdk.Tool{Name: fmt.Sprintf("probe%02d", j), Description: "Record an isolated fixture value"}, func(context.Context, *sdk.CallToolRequest, struct {
					Value string `json:"value"`
				}) (*sdk.CallToolResult, any, error) {
					effects.Add(1)
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "verified business receipt"}}}, nil, nil
				})
			}
			httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
			defer httpServer.Close()
			manager, err := mcp.New(harness.MCPPolicy{AllowHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			req := request("mcp-discovery")
			if err = manager.Bind(ctx, "fixture", req.Session.ID, t.TempDir(), []harness.MCPServer{{Name: "fixture", Transport: "http", URL: httpServer.URL}}); err != nil {
				t.Fatal(err)
			}
			_, _, lease, err := manager.AcquireForTurn(ctx, req.Session.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			fake := &scriptedModel{}
			var approvals []string
			fake.stream = func(_ context.Context, step int, in []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				fake.mu.Lock()
				names := slices.Clone(fake.tools[len(fake.tools)-1])
				fake.mu.Unlock()
				var business []string
				for _, name := range names {
					if strings.HasPrefix(name, "mcp_fixture_") {
						business = append(business, name)
					}
				}
				expected := n
				if n > 10 {
					expected = 0
					if step > 0 {
						expected = 1
					}
				}
				if len(business) != expected || slices.Contains(names, "tool_search") != (n > 10) || slices.Contains(names, "mcp_discover") {
					return nil, fmt.Errorf("step %d offered %d business tools: %v", step, len(business), names)
				}
				finalStep := 1
				if n > 10 {
					finalStep = 2
				}
				if step >= finalStep && n > 0 {
					history, _ := json.Marshal(in)
					if !strings.Contains(string(history), "verified business receipt") {
						return nil, fmt.Errorf("step %d missing original MCP result, calls=%d", step, effects.Load())
					}
				}
				if n == 0 || step >= finalStep {
					return textStream("done"), nil
				}
				name, args := "tool_search", `{"query":"probe07"}`
				if len(business) > 0 {
					for _, s := range business {
						if strings.Contains(s, "probe07_") {
							name = s
						}
					}
					args = `{"value":"once"}`
				}
				return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: fmt.Sprintf("call-%d", step), Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}}}}), nil
			}
			engine := newTestEngine(t, Config{ChatModel: fake, Model: "fixture", MaxIterations: 4, PermissionMode: harness.PermissionModeDangerous, ExtensionFactory: func(context.Context, harness.RunRequest, json.RawMessage) (RunExtensions, error) {
				return RunExtensions{MCP: lease, MCPExposure: "auto"}, nil
			}})
			_, err = engine.Run(ctx, req, func(context.Context, harness.RunEvent) error { return nil }, func(_ context.Context, r harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals = append(approvals, r.ToolName)
				return harness.AllowOnce, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if n > 0 && (effects.Load() != 1 || len(approvals) != 1 || !strings.HasPrefix(approvals[0], "mcp_fixture_probe07_")) {
				t.Fatalf("effects=%d approvals=%v", effects.Load(), approvals)
			}
		})
	}
}
