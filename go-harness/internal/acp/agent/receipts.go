package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const (
	listReceiptsMethod         = "_deerflow/tool_receipts/list"
	reconcileReceiptMethod     = "_deerflow/tool_receipts/reconcile"
	reconciliationRequiredCode = -32010
	receiptConflictCode        = -32011
)

func receiptCapabilities() map[string]any {
	return map[string]any{"version": 1, "listMethod": listReceiptsMethod, "reconcileMethod": reconcileReceiptMethod}
}

func receiptRecoveryError(conflict bool) error {
	code, kind := reconciliationRequiredCode, "reconciliation_required"
	message := "Uncertain tool effects require review. List the tool receipts, verify the external outcome, and explicitly reconcile before sending another prompt."
	if conflict {
		code, kind = receiptConflictCode, "receipt_conflict"
		message = "Tool receipt evidence changed or conflicts with this request. List the receipts again and review the current version; no tool was replayed."
	}
	return &protocol.Error{Code: code, Message: message, Data: map[string]any{"kind": kind, "listMethod": listReceiptsMethod, "reconcileMethod": reconcileReceiptMethod, "automaticReplay": false}}
}

func (a *Agent) receiptRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	invalid := func() (any, error) { return nil, rpcError(protocol.InvalidParams, "Invalid tool receipt parameters") }
	if len(raw) > 128*1024 || rejectDuplicateKeys(raw) != nil {
		return invalid()
	}
	keys := []string{"sessionId"}
	if method == reconcileReceiptMethod {
		keys = append(keys, "review")
	}
	fields, err := strictObject(raw, keys...)
	if err != nil {
		return invalid()
	}
	var sessionID string
	if json.Unmarshal(fields["sessionId"], &sessionID) != nil || !validID(sessionID, 256) {
		return invalid()
	}
	if method == listReceiptsMethod {
		receipts, err := a.service.ListToolReceipts(ctx, a.owner, sessionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"receipts": receipts}, nil
	}
	reviewFields, err := strictObject(fields["review"], "runId", "toolCallId", "expectedVersion", "outcome", "reviewer", "note", "result")
	if err != nil {
		return invalid()
	}
	var review harness.ToolReconciliation
	if err := json.Unmarshal(fields["review"], &review); err != nil {
		return invalid()
	}
	if !validID(review.RunID, 256) || !validID(review.ToolCallID, 1024) || review.ExpectedVersion < 1 || strings.TrimSpace(review.Reviewer) == "" || len(review.Reviewer) > 256 || strings.TrimSpace(review.Note) == "" || len(review.Note) > 16384 || (review.Outcome != harness.ReceiptCompleted && review.Outcome != harness.ReceiptNoEffect) {
		return invalid()
	}
	if result, exists := reviewFields["result"]; exists {
		var parts []json.RawMessage
		trimmed := bytes.TrimSpace(result)
		if len(trimmed) == 0 || trimmed[0] != '[' || json.Unmarshal(result, &parts) != nil || len(parts) > 128 {
			return invalid()
		}
		bytesLeft := 64 * 1024
		for _, part := range parts {
			if _, err := strictObject(part, "type", "text", "uri", "mimeType", "name"); err != nil {
				return invalid()
			}
			var content harness.Content
			if json.Unmarshal(part, &content) != nil {
				return invalid()
			}
			bytesLeft -= len(content.Text) + len(content.URI) + len(content.MimeType) + len(content.Name)
			if bytesLeft < 0 {
				return invalid()
			}
			switch content.Type {
			case "text":
				if content.Text == "" || content.URI != "" || content.MimeType != "" || content.Name != "" {
					return invalid()
				}
			case "resource_link":
				u, parseErr := url.Parse(content.URI)
				if parseErr != nil || !u.IsAbs() || content.Text != "" || len(content.URI) > 4096 || len(content.Name) > 1024 || len(content.MimeType) > 256 {
					return invalid()
				}
			default:
				return invalid()
			}
		}
	}
	receipt, err := a.service.ReconcileToolReceipt(ctx, a.owner, sessionID, review)
	if err != nil {
		return nil, err
	}
	return map[string]any{"receipt": receipt}, nil
}

func validID(value string, max int) bool {
	return value != "" && len(value) <= max && strings.TrimSpace(value) == value
}

// Exact keys reject casing aliases accepted by encoding/json's struct decoder.
// Null values are rejected even for optional evidence fields.
func strictObject(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("expected object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, value := range fields {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("invalid object field")
		}
	}
	return fields, nil
}

func rejectDuplicateKeys(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 16 {
			return fmt.Errorf("too deeply nested")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate key")
				}
				seen[name] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing data")
	}
	return nil
}
