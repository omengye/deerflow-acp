package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestACPModelTransportFailureReturnsSafeContinuation(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, engineFunc(func(_ context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		state := &harness.ExecutionState{SessionID: req.Session.ID, RunID: req.RunID, Status: harness.ExecutionFailed, Version: 145, BlockedReason: "private checkpoint"}
		return harness.RunResult{Execution: state}, fmt.Errorf("private proxy credential=fixture-secret: %w", harness.ErrModelUnavailable)
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "continue"))
	for {
		msg := c.read(t)
		if msg.Method == "session/update" {
			continue
		}
		if string(msg.ID) != strconv.Itoa(id) || msg.Error == nil || msg.Error.Code != -32033 {
			t.Fatalf("unclassified transport failure: %+v", msg)
		}
		data := msg.Error.Data.(map[string]any)
		state := data["execution"].(map[string]any)
		if data["kind"] != "model_transport_failed" || data["automaticReplay"] != false || state["resumable"] != false || state["status"] != "failed" || state["sessionId"] != sid {
			t.Fatalf("lost safe execution state: %+v", data)
		}
		encoded, err := json.Marshal(msg.Error)
		if err != nil || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "fixture-secret") || !strings.Contains(msg.Error.Message, "原会话") || calls.Load() != 1 {
			t.Fatalf("unsafe response or replay: %s calls=%d err=%v", encoded, calls.Load(), err)
		}
		break
	}
}
