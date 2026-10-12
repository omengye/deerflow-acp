package eino

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	nativeMCP "github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
	nativeTools "github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

// Regression coverage uses local fixtures from the original audit.
func TestToolFailureRegressionCommandSpawn(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows executable fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cwd := t.TempDir()
	exe := filepath.Join(cwd, "invalid-fixture.exe")
	if err := os.WriteFile(exe, []byte("This is deliberately not a PE executable."), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.New(ctx, harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	command, err := nativeTools.CommandTool(backend, harness.SandboxLocal)
	if err != nil {
		t.Fatal(err)
	}
	args := `{"executable":` + strconv.Quote(exe) + `}`
	service, session, _ := runtimeWithReceiptInvocation(t, command, args)
	result, runErr := service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "fixture"}}, nil, approveReceiptTool)
	receipts, err := service.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt read: %v count=%d", err, len(receipts))
	}
	_, nextErr := service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, approveReceiptTool)
	t.Logf("AUDIT spawn failure: stopReason=%s runError=%t receiptState=%s nextPromptBlocked=%t", result.StopReason, runErr != nil, receipts[0].State, errors.Is(nextErr, harness.ErrReconciliationRequired))
	if runErr != nil || result.StopReason != "end_turn" || receipts[0].State != harness.ReceiptNotExecuted || nextErr != nil {
		t.Fatal("confirmed failure incorrectly stopped the conversation")
	}
}

func TestToolFailureRegressionMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server := sdk.NewServer(&sdk.Implementation{Name: "audit-error-fixture", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "validate", Description: "Validate fixture input"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "Invalid fixture input; correct arguments and retry."}}}, nil, nil
	})
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true}))
	defer httpServer.Close()
	manager, err := nativeMCP.New(harness.MCPPolicy{AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err = manager.Bind(ctx, "fixture-owner", "fixture-session", t.TempDir(), []harness.MCPServer{{Name: "audit", Transport: "http", URL: httpServer.URL}}); err != nil {
		t.Fatal(err)
	}
	found, err := manager.EnhancedTools(ctx, "fixture-session")
	if err != nil || len(found) != 1 {
		t.Fatalf("tools=%d err=%v", len(found), err)
	}
	var underlying tool.BaseTool = found[0]
	info, err := underlying.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "mcp-failure", Type: "function", Function: schema.FunctionCall{Name: info.Name, Arguments: `{}`}}}}}), nil
		}
		if call == 1 {
			data, _ := json.Marshal(input)
			if !strings.Contains(string(data), "Invalid fixture input") || !strings.Contains(string(data), "Partial effects") {
				t.Error("model did not receive diagnostic and terminal-effect warning")
			}
		}
		return textStream("done"), nil
	}}
	service, session, _ := runtimeWithReceiptModel(t, underlying, model)
	result, runErr := service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "fixture"}}, nil, approveReceiptTool)
	receipts, err := service.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt read: %v count=%d", err, len(receipts))
	}
	_, nextErr := service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, approveReceiptTool)
	evidence, _ := json.Marshal(receipts[0].Result)
	if !strings.Contains(string(evidence), "Invalid fixture input") {
		t.Fatal("MCP business diagnostic was lost")
	}
	t.Logf("REGRESSION MCP isError response: stopReason=%s runError=%t receiptState=%s nextPromptBlocked=%t modelLostDiagnostic=%t", result.StopReason, runErr != nil, receipts[0].State, errors.Is(nextErr, harness.ErrReconciliationRequired), receipts[0].Error == "MCP tool call failed")
	if runErr != nil || result.StopReason != "end_turn" || receipts[0].State != harness.ReceiptFailed || nextErr != nil {
		t.Fatal("confirmed failure incorrectly stopped the conversation")
	}
}

func TestToolFailureRegressionReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	found, cleanup, err := nativeTools.WorkspaceFactory(ctx, harness.RunRequest{Session: harness.Session{CWD: testutil.CanonicalTempDir(t)}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var underlying tool.BaseTool
	for _, candidate := range found {
		info, _ := candidate.Info(ctx)
		if info.Name == "list_directory" {
			underlying = candidate
			break
		}
	}
	if underlying == nil {
		t.Fatal("tool missing")
	}
	service, session, _ := runtimeWithReceiptInvocation(t, underlying, `{"path":"does-not-exist"}`)
	result, runErr := service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "fixture"}}, nil, approveReceiptTool)
	receipts, err := service.ListToolReceipts(ctx, "owner", session.ID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt read: %v count=%d", err, len(receipts))
	}
	t.Logf("AUDIT native list_directory: stopReason=%s runError=%t receiptState=%s", result.StopReason, runErr != nil, receipts[0].State)
	if runErr != nil || result.StopReason != "end_turn" || receipts[0].State != harness.ReceiptNoEffect {
		t.Fatal("confirmed failure incorrectly stopped the conversation")
	}
}
