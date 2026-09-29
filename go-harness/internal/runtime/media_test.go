package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestViewImageTrustedReadOnlyPermissionAndFailureReceipt(t *testing.T) {
	for _, x := range []harness.Session{{Mode: "plan"}, {ApprovalMode: harness.ApprovalReadOnly}} {
		decision, handled := configuredPermission(x, harness.PermissionRequest{ToolName: "view_image"})
		if !handled || decision != harness.AllowOnce {
			t.Fatalf("trusted image inspection denied: %v %v", decision, handled)
		}
		decision, handled = configuredPermission(x, harness.PermissionRequest{ToolName: "mcp/files/view_image"})
		if !handled || decision != harness.RejectOnce {
			t.Fatal("MCP tool impersonated reserved built-in")
		}
	}
	s, req := receiptFixture(t)
	appendReceiptEvent(t, s, req, "tool_start", "image", "view_image")
	appendReceiptEvent(t, s, req, "tool_execute", "image", "view_image")
	end := receiptEvent(req, "tool_end", "image", "view_image")
	end.Status = "failed"
	end.Text = "image cannot be read"
	out, err := s.Store.Append(context.Background(), end)
	if err != nil || out.Receipt.State != harness.ReceiptNoEffect {
		t.Fatalf("read-only failure: %+v %v", out, err)
	}
	if err = s.Store.requireReconciled(context.Background(), req.Session.ID); err != nil {
		t.Fatal("failed image read requires external reconciliation:", err)
	}
}

func TestSDKReviewRejectsForgedMediaWithoutChangingReceipt(t *testing.T) {
	ctx := context.Background()
	s, req := receiptFixture(t)
	appendReceiptEvent(t, s, req, "tool_start", "call", "execute_command")
	appendReceiptEvent(t, s, req, "tool_execute", "call", "execute_command")
	if err := s.Store.Finish(ctx, req.RunID, "cancelled", context.Canceled); err != nil {
		t.Fatal(err)
	}
	original, err := readReceipt(ctx, s.Store.db, req.Session.ID, req.RunID, "call")
	if err != nil {
		t.Fatal(err)
	}
	size := int64(1)
	cases := []harness.Content{
		{Type: "resource_link", URI: "deerflow-asset://other/id"},
		{Type: "resource_link", URI: "file:///evidence.txt", Asset: &harness.AssetRef{SessionID: "other", ID: "forged"}},
		{Type: "text", Text: "evidence", Data: "private-base64"},
		{Type: "image", Data: "private-base64", MimeType: "image/png"},
		{Type: "resource_link", URI: "file:///evidence.txt", Description: strings.Repeat("e", 100)},
		{Type: "resource_link", URI: "file:///evidence.txt", Size: &size},
		{Type: "text", Text: strings.Repeat("e", 64*1024+1)},
	}
	for _, c := range cases {
		review := harness.ToolReconciliation{RunID: req.RunID, ToolCallID: "call", ExpectedVersion: original.Version, Outcome: harness.ReceiptCompleted, Reviewer: "operator", Note: "Verified", Result: []harness.Content{c}}
		if _, err := s.ReconcileToolReceipt(ctx, "owner", req.Session.ID, review); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("forged evidence accepted: %v", err)
		}
	}
	r, err := readReceipt(ctx, s.Store.db, req.Session.ID, req.RunID, "call")
	if err != nil || r.Version != original.Version || r.State != harness.ReceiptUncertain || r.Review != nil {
		t.Fatal("invalid evidence mutated receipt")
	}
	var audits int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM harness_tool_reconciliations`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal("invalid evidence wrote audit")
	}
}
