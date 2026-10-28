package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestModelResponseErrorsArePublicWithoutProviderDetails(t *testing.T) {
	cause := fmt.Errorf("%w: provider credential=fixture-secret in private/workspace", harness.ErrModelResponseInvalid)
	for _, notify := range []bool{false, true} {
		for _, withState := range []bool{false, true} {
			err := error(cause)
			if withState {
				var a Agent
				_, err = a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
					return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionNeedsReconciliation, Version: 7, BlockedReason: "private-checkpoint-path"}}, cause
				})
				if !errors.Is(err, cause) {
					t.Fatal("run response discarded the original error")
				}
			}
			public := mapError(err)
			if notify {
				public = executionUpdateError(err)
			}
			var rpc *protocol.Error
			if !errors.As(public, &rpc) {
				t.Fatalf("missing public error: %v", public)
			}
			encoded, marshalErr := json.Marshal(rpc)
			if marshalErr != nil || rpc.Code != -32026 || rpc.Message == "Internal error" || strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "private") {
				t.Fatalf("unsafe model response diagnostic: %s %v", encoded, marshalErr)
			}
			data, ok := rpc.Data.(map[string]any)
			if !ok || data["kind"] != "model_response_invalid" || data["automaticReplay"] != false {
				t.Fatalf("missing model response classification: %+v", rpc)
			}
			if withState {
				state := data["execution"].(map[string]any)
				if data["getMethod"] != getExecutionMethod || state["runId"] != "run" || state["sessionId"] != "session" || state["status"] != harness.ExecutionNeedsReconciliation || state["version"] != int64(7) || state["resumable"] != false {
					t.Fatalf("lost safe execution state: %+v", data)
				}
			}
		}
	}
	for _, cause := range []error{harness.ErrReconciliationRequired, harness.ErrReceiptConflict, harness.ErrExecutionUnresumable, harness.ErrContextLimit, harness.ErrTaskTreeStopped, harness.ErrPermissionDenied, &harness.CompactionError{Reason: harness.CompactionTruncated}} {
		want := executionUpdateError(cause)
		got := executionUpdateError(errors.Join(harness.ErrModelResponseInvalid, cause))
		if got.Code != want.Code || got.Message != want.Message {
			t.Fatalf("model response classification hid a higher-priority error: got=%+v want=%+v", got, want)
		}
	}
	var value any
	if rpc := executionUpdateError(json.Unmarshal([]byte(`{"tool_argument":`), &value)); rpc.Code != protocol.InternalError {
		t.Fatalf("non-provider JSON error was classified as a model response: %+v", rpc)
	}
}
