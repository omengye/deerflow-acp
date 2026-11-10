package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestACPCheckpointFailureKeepsPartialOutputAndDoesNotReplay(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, engineFunc(func(_ context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		if err := emit(context.Background(), harness.RunEvent{Kind: "text_delta", Text: "Partial result retained."}); err != nil {
			return harness.RunResult{}, err
		}
		state := &harness.ExecutionState{SessionID: req.Session.ID, RunID: req.RunID, Status: harness.ExecutionFailed, Version: 8, BlockedReason: "private-checkpoint"}
		return harness.RunResult{Execution: state}, &harness.CheckpointCommitError{Cause: errors.New("private-store fixture-secret")}
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "continue"))
	sawPartial := false
	for {
		msg := c.read(t)
		if msg.Method == "session/update" {
			sawPartial = sawPartial || strings.Contains(string(msg.Params), "Partial result retained.")
			continue
		}
		if string(msg.ID) != strconv.Itoa(id) || msg.Error == nil || msg.Error.Code != -32030 || !sawPartial {
			t.Fatalf("expected partial output and checkpoint failure: %+v", msg)
		}
		data := msg.Error.Data.(map[string]any)
		state := data["execution"].(map[string]any)
		if data["kind"] != "checkpoint_persist_failed" || data["automaticReplay"] != false || state["sessionId"] != sid || state["resumable"] != false || state["status"] != "failed" {
			t.Fatalf("checkpoint failure lost execution state: %+v", data)
		}
		encoded, err := json.Marshal(msg.Error)
		if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "fixture-secret") || calls.Load() != 1 {
			t.Fatalf("checkpoint error exposed details or replayed execution: %s calls=%d err=%v", encoded, calls.Load(), err)
		}
		break
	}
}
