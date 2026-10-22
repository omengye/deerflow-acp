package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

func TestCancelPermissionThenPromptOnSameConnection(t *testing.T) {
	for _, mode := range []string{"notification_only", "cancelled_response", "selected_response", "permission_only"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"cancel-write\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"cancelled.txt\\\",\\\"content\\\":\\\"must not execute\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Fresh reply\"},\"finish_reason\":\"stop\"}]}\n\n")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer provider.Close()
			f := newFixture(t, nil)
			ledger, err := budget.New(f.db.DB(), budget.Config{})
			if err != nil {
				t.Fatal(err)
			}
			limits := harness.BudgetLimits{MaxModelCalls: 2, MaxToolCalls: 1}
			f.service.Store.BudgetLedger, f.service.Store.BudgetLimits = ledger, limits
			f.service.Engine, err = einoengine.New(context.Background(), einoengine.Config{Provider: "openai", APIKey: "fixture", BaseURL: provider.URL + "/v1", Model: "fixture", DisableSubAgent: true, BudgetLedger: ledger, Budget: limits, CheckpointStore: f.db, SessionStore: f.db, ToolFactory: tools.WorkspaceFactory})
			if err != nil {
				t.Fatal(err)
			}
			c := connect(t, f.service)
			// Cancellation joins checkpoint/receipt persistence before replying.
			// Keep one bounded fixture deadline rather than a five-second timer
			// per frame that can expire during legitimate race-instrumented I/O.
			wait, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			c.waitCtx = wait
			c.initialize(t)
			sid := c.newSession(t, f.cwd)
			first := c.request(t, "session/prompt", promptParams(sid, "write"))
			permission := executionPermissionMessage(t, c)
			var frames []byte
			if mode != "permission_only" {
				cancel, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": sid}})
				if err != nil {
					t.Fatal(err)
				}
				frames = append(cancel, '\n')
			}
			if mode != "notification_only" {
				outcome := "cancelled"
				if mode == "selected_response" {
					outcome = "selected"
				}
				response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": permission.ID, "result": map[string]any{"outcome": map[string]string{"outcome": outcome, "optionId": "allow_once"}}})
				if err != nil {
					t.Fatal(err)
				}
				frames = append(append(frames, response...), '\n')
			}
			if _, err = c.in.Write(frames); err != nil {
				t.Fatal(err)
			}
			want := "cancelled"
			if mode == "permission_only" {
				want = "end_turn"
			}
			stopReason(t, executionPromptResult(t, c, first), want)
			state := executionState(t, c, sid)
			if mode == "permission_only" {
				if state.Status != harness.ExecutionWaitingInput || !state.Resumable {
					t.Fatalf("permission-only dismissal lost resumable wait: %+v", state)
				}
				c.success(t, c.request(t, "_deerflow/executions/cancel", map[string]any{"sessionId": sid, "runId": state.RunID, "expectedVersion": state.Version}))
				state = executionState(t, c, sid)
			}
			if state.Status != harness.ExecutionCancelled || state.Resumable || calls.Load() != 1 {
				t.Fatalf("stop did not settle before response: %+v calls=%d", state, calls.Load())
			}
			next := c.request(t, "session/prompt", promptParams(sid, "fresh question; do not retry the write"))
			stopReason(t, executionPromptResult(t, c, next), "end_turn")
			if calls.Load() != 2 {
				t.Fatalf("unexpected model calls after stop: %d", calls.Load())
			}
			if _, err = os.Stat(filepath.Join(f.cwd, "cancelled.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stopped write executed: %v", err)
			}
			receipts := listWireReceipts(t, c, sid)
			if len(receipts) != 1 || receipts[0].State != harness.ReceiptNotExecuted {
				t.Fatalf("incorrect cancelled receipt: %+v", receipts)
			}
		})
	}
}
