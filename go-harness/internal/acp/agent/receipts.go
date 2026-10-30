package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const (
	listReceiptsMethod         = "_deerflow/tool_receipts/list"
	reconciliationRequiredCode = -32010
	receiptConflictCode        = -32011
)

func receiptCapabilities() map[string]any {
	return map[string]any{"version": 1, "listMethod": listReceiptsMethod, "pagination": "live_keyset"}
}

func receiptRecoveryError(conflict bool) error {
	code, kind := reconciliationRequiredCode, "reconciliation_required"
	message := "The previous execution could not finish safely. Its results were preserved and no tool was replayed."
	if conflict {
		code, kind = receiptConflictCode, "receipt_conflict"
		message = "Tool execution evidence changed or conflicts with this request. Refresh the execution state; no tool was replayed."
	}
	return &protocol.Error{Code: code, Message: message, Data: map[string]any{"kind": kind, "listMethod": listReceiptsMethod, "automaticReplay": false}}
}

func (a *Agent) receiptRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	invalid := func() (any, error) { return nil, rpcError(protocol.InvalidParams, "Invalid tool receipt parameters") }
	if len(raw) > 128*1024 || rejectDuplicateKeys(raw) != nil {
		return invalid()
	}
	fields, err := strictObject(raw, "sessionId", "limit", "cursor", "uncertainOnly")
	if err != nil {
		return invalid()
	}
	var sessionID string
	if json.Unmarshal(fields["sessionId"], &sessionID) != nil || !validID(sessionID, 256) {
		return invalid()
	}
	if method == listReceiptsMethod {
		if _, paged := fields["limit"]; paged {
			var limit int
			var cursor string
			var uncertainOnly bool
			if json.Unmarshal(fields["limit"], &limit) != nil || limit < 1 || limit > 20 {
				return invalid()
			}
			if v, ok := fields["cursor"]; ok && (json.Unmarshal(v, &cursor) != nil || len(cursor) > 8192) {
				return invalid()
			}
			if v, ok := fields["uncertainOnly"]; ok && json.Unmarshal(v, &uncertainOnly) != nil {
				return invalid()
			}
			return a.service.ToolReceiptPage(ctx, a.owner, sessionID, limit, cursor, uncertainOnly)
		}
		if len(fields) != 1 {
			return invalid()
		}
		receipts, err := a.service.ListToolReceipts(ctx, a.owner, sessionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"receipts": receipts}, nil
	}
	return nil, rpcError(protocol.MethodNotFound, "Method not found")
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
