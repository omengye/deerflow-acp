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

func TestReceiptExtensionRecoveryAndAtomicReviewOverProtocol(t *testing.T) {
	var effects atomic.Int32
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if req.Input[0].Text == "new request" {
			return harness.RunResult{StopReason: "end_turn"}, nil
		}
		for _, e := range []harness.RunEvent{{Kind: "tool_start", ToolCallID: "call", ToolName: "write_file", Status: "pending", Arguments: json.RawMessage(`{"path":"result.txt"}`)}, {Kind: "tool_execute", ToolCallID: "call", ToolName: "write_file", Status: "in_progress"}} {
			if err := emit(ctx, e); err != nil {
				return harness.RunResult{}, err
			}
		}
		effects.Add(1)
		if err := emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: "call", ToolName: "write_file", Status: "failed", Content: []harness.Content{{Type: "text", Text: "operation result requires review"}}}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{}, errors.New("private-provider-token")
	}))
	c := connect(t, f.service)
	initID := c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var init struct {
		Meta struct {
			Deerflow struct {
				ToolReceipts struct {
					Version         int    `json:"version"`
					ListMethod      string `json:"listMethod"`
					ReconcileMethod string `json:"reconcileMethod"`
				} `json:"toolReceipts"`
			} `json:"deerflow"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(c.success(t, initID), &init); err != nil {
		t.Fatal(err)
	}
	if caps := init.Meta.Deerflow.ToolReceipts; caps.Version != 1 || caps.ListMethod != receiptListMethod || caps.ReconcileMethod != receiptReviewMethod {
		t.Fatalf("receipt capability=%+v", caps)
	}
	sid := c.newSession(t, f.cwd)
	prompt := c.request(t, "session/prompt", promptParams(sid, "uncertain effect"))
	update(t, c.read(t), sid, "tool_call")
	update(t, c.read(t), sid, "tool_call_update")
	update(t, c.read(t), sid, "tool_call_update")
	if msg := c.response(t, prompt); msg.Error == nil || msg.Error.Code != protocol.InternalError || strings.Contains(msg.Error.Message, "private-provider-token") {
		t.Fatalf("unsafe run error=%+v", msg)
	}
	blocked := c.request(t, "session/prompt", promptParams(sid, "new request"))
	msg := c.response(t, blocked)
	if msg.Error == nil || msg.Error.Code != -32010 || !strings.Contains(msg.Error.Message, "review") {
		t.Fatalf("unactionable reconciliation error=%+v", msg)
	}
	data, _ := json.Marshal(msg.Error.Data)
	if !strings.Contains(string(data), receiptListMethod) || !strings.Contains(string(data), receiptReviewMethod) {
		t.Fatalf("recovery methods missing: %s", data)
	}
	receipts := listWireReceipts(t, c, sid)
	if len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain {
		t.Fatalf("receipts=%+v", receipts)
	}
	r := receipts[0]
	other := connect(t, f.service)
	other.initialize(t)
	foreign := other.request(t, receiptListMethod, map[string]any{"sessionId": sid})
	if msg := other.response(t, foreign); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("foreign list=%+v", msg)
	}
	review := harness.ToolReconciliation{RunID: r.RunID, ToolCallID: r.ToolCallID, ExpectedVersion: r.Version, Outcome: harness.ReceiptCompleted, Reviewer: "operator", Note: "Checked result.txt and confirmed the write completed.", Result: []harness.Content{{Type: "resource_link", URI: "file:///workspace/result.txt"}}}
	params := map[string]any{"sessionId": sid, "review": review}
	foreign = other.request(t, receiptReviewMethod, params)
	if msg := other.response(t, foreign); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("foreign review=%+v", msg)
	}
	c.notify(t, receiptReviewMethod, params)
	if got := listWireReceipts(t, c, sid); got[0].State != harness.ReceiptUncertain {
		t.Fatal("notification changed a receipt")
	}
	stale := review
	stale.ExpectedVersion++
	id := c.request(t, receiptReviewMethod, map[string]any{"sessionId": sid, "review": stale})
	if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != -32011 {
		t.Fatalf("stale review=%+v", msg)
	}
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_review BEFORE INSERT ON harness_tool_reconciliations BEGIN SELECT RAISE(ABORT,'private-database-credential'); END`); err != nil {
		t.Fatal(err)
	}
	id = c.request(t, receiptReviewMethod, params)
	if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != protocol.InternalError || strings.Contains(msg.Error.Message, "private-database") {
		t.Fatalf("unsafe review failure=%+v", msg)
	}
	if got := listWireReceipts(t, c, sid); got[0].State != harness.ReceiptUncertain || got[0].Version != r.Version {
		t.Fatal("failed review was not atomic")
	}
	if _, err := f.db.DB().Exec(`DROP TRIGGER fail_review`); err != nil {
		t.Fatal(err)
	}
	id = c.request(t, receiptReviewMethod, params)
	var result struct {
		Receipt harness.ToolReceipt `json:"receipt"`
	}
	if err := json.Unmarshal(c.success(t, id), &result); err != nil {
		t.Fatal(err)
	}
	if result.Receipt.State != harness.ReceiptCompleted || result.Receipt.Review == nil || result.Receipt.Review.Note != review.Note || effects.Load() != 1 {
		t.Fatalf("review=%+v effects=%d", result, effects.Load())
	}
	id = c.request(t, receiptReviewMethod, params)
	if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != -32011 {
		t.Fatalf("repeat review=%+v", msg)
	}
	id = c.request(t, "session/prompt", promptParams(sid, "new request"))
	stopReason(t, c.success(t, id), "end_turn")
	if effects.Load() != 1 {
		t.Fatal("review replayed the external operation")
	}
}

func TestReceiptExtensionsRequireIdleLease(t *testing.T) {
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
		t.Fatal("prompt did not start")
	}
	for _, method := range []string{receiptListMethod, receiptReviewMethod} {
		params := map[string]any{"sessionId": sid}
		if method == receiptReviewMethod {
			params["review"] = harness.ToolReconciliation{RunID: "run", ToolCallID: "call", ExpectedVersion: 1, Outcome: harness.ReceiptNoEffect, Reviewer: "operator", Note: "Reviewed."}
		}
		id := c.request(t, method, params)
		if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
			t.Fatalf("busy %s=%+v", method, msg)
		}
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	stopReason(t, c.success(t, prompt), "cancelled")
}

func TestReceiptExtensionStrictParameters(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		t.Error("validation executed engine")
		return harness.RunResult{}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	for _, raw := range []string{`{}`, `{"sessionId":null}`, `{"sessionId":123}`, `{"SessionId":"` + sid + `"}`, `{"sessionId":"` + sid + `","extra":"private-value"}`, `{"sessionId":"` + sid + `","sessionId":"private-value"}`} {
		id := c.request(t, receiptListMethod, json.RawMessage(raw))
		if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != protocol.InvalidParams || strings.Contains(msg.Error.Message, "private-value") {
			t.Fatalf("list invalid params=%+v", msg)
		}
	}
	base := `"runId":"run","toolCallId":"call","expectedVersion":1,"outcome":"no_effect","reviewer":"operator","note":"Reviewed"`
	for _, review := range []string{`null`, `{}`, `{` + base + `,"unknown":"private-value"}`, `{` + base + `,"expectedVersion":2}`, `{` + strings.Replace(base, `"expectedVersion":1`, `"expectedVersion":"1"`, 1) + `}`, `{` + strings.Replace(base, `"expectedVersion":1`, `"expectedVersion":1.5`, 1) + `}`, `{` + strings.Replace(base, `"note":"Reviewed"`, `"note":null`, 1) + `}`, `{` + base + `,"result":[{"type":"image","data":"private-value"}]}`, `{` + base + `,"result":[{"type":"text","text":"one","text":"two"}]}`, `{` + base + `,"result":[{"type":"resource_link","uri":"relative-path"}]}`, `{` + base + `,"result":null}`, `{` + strings.Replace(base, "Reviewed", strings.Repeat("x", 129*1024), 1) + `}`} {
		raw := fmt.Sprintf(`{"sessionId":%q,"review":%s}`, sid, review)
		id := c.request(t, receiptReviewMethod, json.RawMessage(raw))
		if msg := c.response(t, id); msg.Error == nil || msg.Error.Code != protocol.InvalidParams || strings.Contains(msg.Error.Message, "private-value") {
			t.Fatalf("review invalid params=%+v", msg)
		}
	}
	unknown := c.request(t, "_deerflow/tool_receipts/execute", map[string]any{"sessionId": sid})
	if msg := c.response(t, unknown); msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
		t.Fatalf("unknown extension=%+v", msg)
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
