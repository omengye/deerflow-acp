package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

func executionPermissionMessage(t *testing.T, c *client) wireMessage {
	t.Helper()
	for {
		msg := c.read(t)
		if msg.Method == "session/request_permission" {
			return msg
		}
		if msg.Method != "session/update" {
			t.Fatalf("expected permission, received %+v", msg)
		}
	}
}

func executionPromptResult(t *testing.T, c *client, id int) json.RawMessage {
	t.Helper()
	for {
		msg := c.read(t)
		if msg.Method == "session/update" {
			continue
		}
		if msg.Method != "" || string(msg.ID) != strconv.Itoa(id) || msg.Error != nil {
			t.Fatalf("expected prompt result %d, received %+v", id, msg)
		}
		return msg.Result
	}
}

func executionState(t *testing.T, c *client, sessionID string) harness.ExecutionState {
	t.Helper()
	var state harness.ExecutionState
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/executions/get", map[string]any{"sessionId": sessionID})), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestACPExecutionDisconnectResumeAndCancel(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelWaiting), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"acp-write\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"approved.txt\\\",\\\"content\\\":\\\"approved\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done\"},\"finish_reason\":\"stop\"}]}\n\n")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer provider.Close()
			f := newFixture(t, nil)
			ctx := context.Background()
			ledger, err := budget.New(f.db.DB(), budget.Config{})
			if err != nil {
				t.Fatal(err)
			}
			limits := harness.BudgetLimits{MaxModelCalls: 2, MaxToolCalls: 1}
			f.service.Store.BudgetLedger, f.service.Store.BudgetLimits = ledger, limits
			f.service.Engine, err = einoengine.New(ctx, einoengine.Config{Provider: "openai", APIKey: "fixture", BaseURL: provider.URL + "/v1", Model: "fixture", DisableSubAgent: true, BudgetLedger: ledger, Budget: limits, CheckpointStore: f.db, SessionStore: f.db, ToolFactory: tools.WorkspaceFactory})
			if err != nil {
				t.Fatal(err)
			}
			c := connect(t, f.service)
			init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
			if !bytes.Contains(init, []byte(`"resumeMethod":"_deerflow/executions/resume"`)) {
				t.Fatalf("missing execution capability: %s", init)
			}
			sid := c.newSession(t, f.cwd)
			c.request(t, "session/prompt", promptParams(sid, "write"))
			executionPermissionMessage(t, c)
			before := executionState(t, c, sid)
			if before.Status != harness.ExecutionWaitingInput || !before.Resumable || len(before.WaitingInputs) != 1 {
				t.Fatalf("waiting=%+v", before)
			}
			if _, err := os.Stat(filepath.Join(f.cwd, "approved.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("premature effect: %v", err)
			}
			stranger := connect(t, f.service)
			stranger.initialize(t)
			if msg := stranger.response(t, stranger.request(t, "_deerflow/executions/get", map[string]any{"sessionId": sid})); msg.Error == nil {
				t.Fatal("foreign execution query accepted")
			}
			if err := c.agent.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-c.finished:
			case <-time.After(5 * time.Second):
				t.Fatal("disconnect did not finish")
			}
			c2 := connect(t, f.service)
			c2.initialize(t)
			c2.success(t, c2.request(t, "session/resume", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}}))
			state := executionState(t, c2, sid)
			if state.RunID != before.RunID || state.Version != before.Version || state.Status != harness.ExecutionWaitingInput || calls.Load() != 1 {
				t.Fatalf("after reconnect=%+v calls=%d", state, calls.Load())
			}
			for _, params := range []map[string]any{
				{"sessionId": sid, "runId": state.RunID, "expectedVersion": state.Version, "targets": map[string]any{}},
				{"sessionId": sid, "runId": state.RunID, "expectedVersion": state.Version, "prompt": []any{}},
				{"sessionId": sid, "runId": state.RunID},
			} {
				msg := c2.response(t, c2.request(t, "_deerflow/executions/resume", params))
				if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
					t.Fatalf("invalid request=%+v", msg)
				}
			}
			msg := c2.response(t, c2.request(t, "session/prompt", promptParams(sid, "not a resume")))
			if msg.Error == nil || msg.Error.Code != -32012 {
				t.Fatalf("waiting error=%+v", msg)
			}
			msg = c2.response(t, c2.request(t, "_deerflow/executions/resume", map[string]any{"sessionId": sid, "runId": state.RunID, "expectedVersion": state.Version + 1}))
			if msg.Error == nil || msg.Error.Code != -32013 {
				t.Fatalf("stale version=%+v", msg)
			}
			params := map[string]any{"sessionId": sid, "runId": state.RunID, "expectedVersion": state.Version}
			if cancelWaiting {
				c2.success(t, c2.request(t, "_deerflow/executions/cancel", params))
				state = executionState(t, c2, sid)
				if state.Status != harness.ExecutionCancelled || calls.Load() != 1 {
					t.Fatalf("cancelled=%+v calls=%d", state, calls.Load())
				}
			} else {
				resumeID := c2.request(t, "_deerflow/executions/resume", params)
				permission := executionPermissionMessage(t, c2)
				// Resume reserves the session on the protocol reader before its
				// asynchronous handler asks the client to grant permission.
				msg := c2.response(t, c2.request(t, "session/prompt", promptParams(sid, "conflicting")))
				if msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
					t.Fatalf("resume admission=%+v", msg)
				}
				c2.send(t, map[string]any{"jsonrpc": "2.0", "id": permission.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}})
				result := executionPromptResult(t, c2, resumeID)
				if !bytes.Contains(result, []byte(`"status":"completed"`)) {
					t.Fatalf("resume metadata=%s", result)
				}
				state = executionState(t, c2, sid)
				if state.Status != harness.ExecutionCompleted || state.Attempt != 2 || calls.Load() != 2 {
					t.Fatalf("completed=%+v calls=%d", state, calls.Load())
				}
				data, err := os.ReadFile(filepath.Join(f.cwd, "approved.txt"))
				if err != nil || string(data) != "approved" {
					t.Fatalf("file=%q err=%v", data, err)
				}
			}
			if state.RunID != before.RunID || state.InputID != before.InputID {
				t.Fatal("resume replaced accepted input")
			}
			receipts := listWireReceipts(t, c2, sid)
			want := harness.ReceiptCompleted
			if cancelWaiting {
				want = harness.ReceiptNotExecuted
			}
			if len(receipts) != 1 || receipts[0].State != want {
				t.Fatalf("receipts=%+v", receipts)
			}
		})
	}
}
