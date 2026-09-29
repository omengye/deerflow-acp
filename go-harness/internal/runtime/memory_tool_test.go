package runtime

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMemorySearchIsTrustedOnlyByItsBuiltInName(t *testing.T) {
	for _, session := range []harness.Session{{Mode: "plan"}, {ApprovalMode: harness.ApprovalReadOnly}} {
		decision, handled := configuredPermission(session, harness.PermissionRequest{ToolName: "search_memory"})
		if !handled || decision != harness.AllowOnce {
			t.Fatalf("read-only memory search denied: %s %v", decision, handled)
		}
		decision, handled = configuredPermission(session, harness.PermissionRequest{ToolName: "mcp_memory_search"})
		if !handled || decision != harness.RejectOnce {
			t.Fatal("remote MCP tool gained built-in memory permission")
		}
	}
	s, req := receiptFixture(t)
	appendReceiptEvent(t, s, req, "tool_start", "memory", "search_memory")
	appendReceiptEvent(t, s, req, "tool_execute", "memory", "search_memory")
	end := receiptEvent(req, "tool_end", "memory", "search_memory")
	end.Status = "failed"
	end.Text = "memory search failed"
	out, err := s.Store.Append(context.Background(), end)
	if err != nil || out.Receipt == nil || out.Receipt.State != harness.ReceiptNoEffect {
		t.Fatalf("read-only failure state: %+v %v", out, err)
	}
}
