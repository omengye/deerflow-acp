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

func TestACPModelRateLimitRetainsPartialOutputAndSafeState(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		if err := emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "Checking the next result."}); err != nil {
			return harness.RunResult{}, err
		}
		state := &harness.ExecutionState{SessionID: req.Session.ID, RunID: req.RunID, Status: harness.ExecutionFailed, Version: 14, BlockedReason: "private-native-checkpoint"}
		return harness.RunResult{Execution: state}, fmt.Errorf("private-upstream-detail credential=fixture-secret: %w", harness.ErrModelRateLimited)
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
		if string(msg.ID) != strconv.Itoa(id) || msg.Error == nil || msg.Error.Code != -32027 || !sawText {
			t.Fatalf("expected partial output followed by classified failure: %+v, sawText=%v", msg, sawText)
		}
		data := msg.Error.Data.(map[string]any)
		state := data["execution"].(map[string]any)
		if data["kind"] != "model_rate_limited" || data["automaticReplay"] != false || data["getMethod"] != "_deerflow/executions/get" || state["sessionId"] != sid || state["runId"] == "" || state["resumable"] != false || state["status"] != "failed" || state["version"] != float64(14) {
			t.Fatalf("lost safe recovery metadata: %+v", data)
		}
		encoded, err := json.Marshal(msg.Error)
		if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "fixture-secret") || !strings.Contains(msg.Error.Message, "模型服务请求受到限流") {
			t.Fatalf("unsafe or unclassified diagnostic over ACP: %s, %v", encoded, err)
		}
		if calls.Load() != 1 {
			t.Fatalf("ACP replayed the failed execution: %d calls", calls.Load())
		}
		break
	}
}
