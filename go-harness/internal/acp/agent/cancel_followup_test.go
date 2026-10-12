package agent_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestCancelToolThenPromptAgainOnSameConnection(t *testing.T) {
	started, cleanup := make(chan struct{}), make(chan struct{})
	var cleanupOnce sync.Once
	defer cleanupOnce.Do(func() { close(cleanup) })
	var effects atomic.Int32
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if req.Input[0].Text != "start effect" {
			return harness.RunResult{StopReason: "end_turn"}, nil
		}
		for _, event := range []harness.RunEvent{
			{Kind: "tool_start", ToolCallID: "stopped", ToolName: "execute", Status: "pending", Arguments: json.RawMessage(`{}`)},
			{Kind: "tool_execute", ToolCallID: "stopped", ToolName: "execute", Status: "in_progress"},
		} {
			if err := emit(ctx, event); err != nil {
				return harness.RunResult{}, err
			}
		}
		effects.Add(1)
		close(started)
		<-ctx.Done()
		<-cleanup
		return harness.RunResult{}, ctx.Err()
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	first := c.request(t, "session/prompt", promptParams(sid, "start effect"))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("effect did not start")
	}
	update(t, c.read(t), sid, "tool_call")
	update(t, c.read(t), sid, "tool_call_update")
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	early := c.request(t, "session/prompt", promptParams(sid, "before cleanup"))
	if response := c.response(t, early); response.Error == nil || response.Error.Code != protocol.ServerBusy {
		t.Fatalf("new run entered before cleanup: %+v", response)
	}
	cleanupOnce.Do(func() { close(cleanup) })
	stopReason(t, executionPromptResult(t, c, first), "cancelled")
	for range 2 {
		next := c.request(t, "session/prompt", promptParams(sid, "new question"))
		stopReason(t, executionPromptResult(t, c, next), "end_turn")
	}
	receipts := listWireReceipts(t, c, sid)
	if len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain || receipts[0].Review != nil || effects.Load() != 1 {
		t.Fatalf("stopped effect replayed or falsely reviewed: %+v effects=%d", receipts, effects.Load())
	}
}
