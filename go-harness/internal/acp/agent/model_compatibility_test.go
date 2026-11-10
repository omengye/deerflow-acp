package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func TestModelToolsCompatibilityPublicErrorPreservesSafeState(t *testing.T) {
	cause := fmt.Errorf("%w: private-provider-message fixture-secret", harness.ErrModelToolsRequireResponses)
	var a Agent
	_, failure := a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
		return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionFailed, Version: 14, BlockedReason: "private-checkpoint"}}, cause
	})
	for _, err := range []error{mapError(failure), executionUpdateError(failure)} {
		var rpc *protocol.Error
		if !errors.As(err, &rpc) {
			t.Fatal("missing protocol error")
		}
		encoded, marshalErr := json.Marshal(rpc)
		if marshalErr != nil || rpc.Code != -32031 || !strings.Contains(rpc.Message, "Responses") || !strings.Contains(rpc.Message, "切换") || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "fixture-secret") {
			t.Fatalf("unsafe or unhelpful compatibility diagnostic: %s, %v", encoded, marshalErr)
		}
		data := rpc.Data.(map[string]any)
		state := data["execution"].(map[string]any)
		if data["kind"] != "model_tools_require_responses" || data["automaticReplay"] != false || data["getMethod"] != getExecutionMethod || state["sessionId"] != "session" || state["runId"] != "run" || state["resumable"] != false || state["version"] != int64(14) {
			t.Fatalf("lost safe execution metadata: %+v", data)
		}
	}
}

func TestModelToolsCompatibilityPreservesHigherPriorityFailures(t *testing.T) {
	for _, cause := range []error{harness.ErrReconciliationRequired, harness.ErrReceiptConflict, harness.ErrExecutionUnresumable, harness.ErrContextLimit, harness.ErrTaskTreeStopped, harness.ErrPermissionDenied, harness.ErrCheckpointCommit, harness.ErrModelResponseInvalid, &harness.CompactionError{Reason: harness.CompactionTruncated}} {
		want := executionUpdateError(cause)
		got := executionUpdateError(errors.Join(harness.ErrModelToolsRequireResponses, cause))
		if got.Code != want.Code || got.Message != want.Message {
			t.Fatalf("compatibility hid higher-priority error: got=%+v want=%+v", got, want)
		}
	}
	cause := errors.Join(harness.ErrModelToolsRequireResponses, &budget.PersistenceError{Operation: "settle", Err: errors.New("private-database fixture-secret")})
	var a Agent
	_, failure := a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
		return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionFailed, Version: 14}}, cause
	})
	rpc := executionUpdateError(failure)
	encoded, _ := json.Marshal(rpc)
	if rpc.Code != protocol.InternalError || rpc.Data.(map[string]any)["execution"] == nil || strings.Contains(string(encoded), "model_tools_require_responses") || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "fixture-secret") {
		t.Fatalf("compatibility hid persistence failure or exposed private details: %s", encoded)
	}
}
