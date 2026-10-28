package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestACPModelResponseFailureRetainsPartialOutputAndSafeState(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "Checking the next result."}); err != nil {
			return harness.RunResult{}, err
		}
		state := &harness.ExecutionState{SessionID: req.Session.ID, RunID: req.RunID, Status: harness.ExecutionNeedsReconciliation, Version: 8, BlockedReason: "private-native-checkpoint"}
		return harness.RunResult{Execution: state}, fmt.Errorf("private-upstream-detail: %w", harness.ErrModelResponseInvalid)
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "continue the task"))
	sawText := false
	for {
		msg := c.read(t)
		if msg.Method == "session/update" {
			sawText = sawText || strings.Contains(string(msg.Params), "Checking the next result.")
			continue
		}
		if string(msg.ID) != strconv.Itoa(id) || msg.Error == nil || msg.Error.Code != -32026 || !sawText {
			t.Fatalf("expected partial output followed by classified failure: %+v, sawText=%v", msg, sawText)
		}
		data := msg.Error.Data.(map[string]any)
		state := data["execution"].(map[string]any)
		if data["kind"] != "model_response_invalid" || data["automaticReplay"] != false || state["sessionId"] != sid || state["runId"] == "" || state["resumable"] != false || state["status"] != "needs_reconciliation" || state["version"] != float64(8) {
			t.Fatalf("lost safe recovery metadata: %+v", data)
		}
		encoded, err := json.Marshal(msg.Error)
		if err != nil || strings.Contains(string(encoded), "private-") || msg.Error.Message == "Internal error" {
			t.Fatalf("unsafe or unclassified diagnostic over ACP: %s, %v", encoded, err)
		}
		break
	}
}
