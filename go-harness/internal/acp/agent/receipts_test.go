package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const receiptListMethod = "_deerflow/tool_receipts/list"
const receiptReviewMethod = "_deerflow/tool_receipts/reconcile"

func TestReceiptPagedRecoveryKeepsLaterUncertainCallsReachable(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for _, id := range []string{"a", "b"} {
			for _, event := range []harness.RunEvent{{Kind: "tool_start", ToolCallID: id, ToolName: "write_file", Status: "pending", Arguments: json.RawMessage(`{}`)}, {Kind: "tool_execute", ToolCallID: id, ToolName: "write_file", Status: "in_progress"}, {Kind: "tool_end", ToolCallID: id, ToolName: "write_file", Status: "failed"}} {
				if err := emit(ctx, event); err != nil {
					return harness.RunResult{}, err
				}
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "fixture"))
	for range 2 {
		update(t, c.read(t), sid, "tool_call")
		update(t, c.read(t), sid, "tool_call_update")
		update(t, c.read(t), sid, "tool_call_update")
	}
	c.success(t, id)
	var page struct {
		Receipts   []harness.ToolReceipt `json:"receipts"`
		NextCursor string                `json:"nextCursor"`
		HasMore    bool                  `json:"hasMore"`
	}
	query := map[string]any{"sessionId": sid, "limit": 1, "uncertainOnly": true}
	id = c.request(t, receiptListMethod, query)
	if err := json.Unmarshal(c.success(t, id), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Receipts) != 1 || page.Receipts[0].ToolCallID != "a" || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}
	query["cursor"] = page.NextCursor
	id = c.request(t, receiptListMethod, query)
	if err := json.Unmarshal(c.success(t, id), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Receipts) != 1 || page.Receipts[0].ToolCallID != "b" || page.HasMore {
		t.Fatalf("later uncertain call hidden: %+v", page)
	}
	query["uncertainOnly"] = false
	id = c.request(t, receiptListMethod, query)
	if result := c.response(t, id); result.Error == nil {
		t.Fatal("cursor accepted with a different filter")
	}
}

func listWireReceipts(t *testing.T, c *client, sid string) []harness.ToolReceipt {
	t.Helper()
	id := c.request(t, receiptListMethod, map[string]any{"sessionId": sid})
	var result struct {
		Receipts []harness.ToolReceipt `json:"receipts"`
	}
	if err := json.Unmarshal(c.success(t, id), &result); err != nil {
		t.Fatal(err)
	}
	return result.Receipts
}

func TestReceiptExtensionIsReadOnlyAndFailedTurnDoesNotReplay(t *testing.T) {
	var effects atomic.Int32
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if req.Input[0].Text == "new request" {
			return harness.RunResult{StopReason: "end_turn"}, nil
		}
		for _, e := range []harness.RunEvent{
			{Kind: "tool_start", ToolCallID: "call", ToolName: "write_file", Status: "pending", Arguments: json.RawMessage("{\"path\":\"result.txt\"}")},
			{Kind: "tool_execute", ToolCallID: "call", ToolName: "write_file", Status: "in_progress"},
		} {
			if err := emit(ctx, e); err != nil {
				return harness.RunResult{}, err
			}
		}
		effects.Add(1)
		if err := emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: "call", ToolName: "write_file", Status: "failed", Content: []harness.Content{{Type: "text", Text: "outcome unconfirmed"}}}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{}, errors.New("private-provider-token")
	}))
	c := connect(t, f.service)
	initID := c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var init map[string]any
	if err := json.Unmarshal(c.success(t, initID), &init); err != nil {
		t.Fatal(err)
	}
	caps := init["_meta"].(map[string]any)["deerflow"].(map[string]any)["toolReceipts"].(map[string]any)
	if caps["listMethod"] != receiptListMethod || caps["reconcileMethod"] != nil {
		t.Fatalf("receipt capabilities: %+v", caps)
	}
	sid := c.newSession(t, f.cwd)
	prompt := c.request(t, "session/prompt", promptParams(sid, "uncertain effect"))
	update(t, c.read(t), sid, "tool_call")
	update(t, c.read(t), sid, "tool_call_update")
	update(t, c.read(t), sid, "tool_call_update")
	if msg := c.response(t, prompt); msg.Error == nil || msg.Error.Code != protocol.InternalError || strings.Contains(msg.Error.Message, "private-provider-token") {
		t.Fatalf("unsafe error: %+v", msg)
	}
	before := listWireReceipts(t, c, sid)
	if len(before) != 1 || before[0].State != harness.ReceiptUncertain {
		t.Fatalf("receipt: %+v", before)
	}
	retired := c.request(t, receiptReviewMethod, map[string]any{"sessionId": sid})
	if msg := c.response(t, retired); msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
		t.Fatalf("manual review still available: %+v", msg)
	}
	fresh := c.request(t, "session/prompt", promptParams(sid, "new request"))
	stopReason(t, c.success(t, fresh), "end_turn")
	after := listWireReceipts(t, c, sid)
	if effects.Load() != 1 || len(after) != 1 || after[0].State != before[0].State || after[0].Version != before[0].Version || after[0].Review != nil {
		t.Fatalf("history changed or replayed: %+v", after)
	}
	other := connect(t, f.service)
	other.initialize(t)
	foreign := other.request(t, receiptListMethod, map[string]any{"sessionId": sid})
	if msg := other.response(t, foreign); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("foreign read: %+v", msg)
	}
}

func TestReceiptListRequiresIdleLease(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		close(started)
		<-ctx.Done()
		return harness.RunResult{}, ctx.Err()
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	prompt := c.request(t, "session/prompt", promptParams(sid, "wait"))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("not started")
	}
	id := c.request(t, receiptListMethod, map[string]any{"sessionId": sid})
	if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("busy read: %+v", msg)
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	stopReason(t, c.success(t, prompt), "cancelled")
}

func TestReceiptListStrictParameters(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		t.Error("executed engine")
		return harness.RunResult{}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	for _, raw := range []string{"{}", "{\"sessionId\":null}", "{\"sessionId\":123}", fmt.Sprintf("{\"SessionId\":%q}", sid), fmt.Sprintf("{\"sessionId\":%q,\"extra\":\"private-value\"}", sid), fmt.Sprintf("{\"sessionId\":%q,\"sessionId\":\"private-value\"}", sid)} {
		id := c.request(t, receiptListMethod, json.RawMessage(raw))
		if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != protocol.InvalidParams || strings.Contains(msg.Error.Message, "private-value") {
			t.Fatalf("invalid params: %+v", msg)
		}
	}
}

func TestDomainErrorProjectionDoesNotLeakJoinedDiagnostics(t *testing.T) {
	for _, domain := range []error{harness.ErrInvalidInput, harness.ErrBusy, harness.ErrReceiptConflict, harness.ErrReconciliationRequired} {
		t.Run(domain.Error(), func(t *testing.T) {
			f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
				return harness.RunResult{}, errors.Join(domain, errors.New("private-diagnostic-secret"))
			}))
			c := connect(t, f.service)
			c.initialize(t)
			sid := c.newSession(t, f.cwd)
			id := c.request(t, "session/prompt", promptParams(sid, "error"))
			msg := c.response(t, id)
			data, _ := json.Marshal(msg.Error)
			if msg.Error == nil || strings.Contains(string(data), "private-diagnostic-secret") {
				t.Fatalf("unsafe error projection=%s", data)
			}
		})
	}
}
