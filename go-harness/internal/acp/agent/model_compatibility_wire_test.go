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

func TestACPModelToolsCompatibilityExplainsInterfaceWithoutReplay(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, engineFunc(func(_ context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		state := &harness.ExecutionState{SessionID: req.Session.ID, RunID: req.RunID, Status: harness.ExecutionFailed, Version: 14, BlockedReason: "private-native-checkpoint"}
		return harness.RunResult{Execution: state}, fmt.Errorf("private-provider-detail fixture-secret: %w", harness.ErrModelToolsRequireResponses)
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "continue the task"))
	for {
		msg := c.read(t)
		if msg.Method == "session/update" {
			continue
		}
		if string(msg.ID) != strconv.Itoa(id) || msg.Error == nil || msg.Error.Code != -32031 {
			t.Fatalf("expected classified interface error instead of Internal error: %+v", msg)
		}
		data := msg.Error.Data.(map[string]any)
		state := data["execution"].(map[string]any)
		encoded, err := json.Marshal(msg.Error)
		if err != nil || !strings.Contains(msg.Error.Message, "Responses") || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "fixture-secret") {
			t.Fatalf("unsafe compatibility diagnostic over ACP: %s, %v", encoded, err)
		}
		if data["kind"] != "model_tools_require_responses" || data["automaticReplay"] != false || data["getMethod"] != "_deerflow/executions/get" || state["sessionId"] != sid || state["runId"] == "" || state["resumable"] != false || state["status"] != "failed" || calls.Load() != 1 {
			t.Fatalf("lost execution state or replayed a failed run: %+v, calls=%d", data, calls.Load())
		}
		break
	}
}
