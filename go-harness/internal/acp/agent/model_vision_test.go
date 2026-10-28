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

func TestModelVisionFailureHasSafeRecoveryGuidance(t *testing.T) {
	cause := fmt.Errorf("private-provider-detail: %w", harness.ErrModelVisionUnavailable)
	if !errors.Is(cause, harness.ErrInvalidInput) {
		t.Fatal("vision error lost invalid-input compatibility")
	}
	for _, withState := range []bool{false, true} {
		err := error(cause)
		if withState {
			var a Agent
			_, err = a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
				return harness.RunResult{Execution: &harness.ExecutionState{
					SessionID: "session", RunID: "run", Status: harness.ExecutionNeedsReconciliation,
					Version: 7, BlockedReason: "private-checkpoint-path",
				}}, cause
			})
			if !errors.Is(err, harness.ErrModelVisionUnavailable) {
				t.Fatal("run response discarded vision error classification")
			}
		}
		for _, public := range []error{mapError(err), executionUpdateError(err)} {
			var rpc *protocol.Error
			if !errors.As(public, &rpc) || rpc.Code != protocol.InvalidParams || !strings.Contains(rpc.Message, "会话历史") || !strings.Contains(rpc.Message, "模型设置") {
				t.Fatalf("missing actionable vision error: %+v", rpc)
			}
			data, ok := rpc.Data.(map[string]any)
			if !ok || data["kind"] != "model_vision_unavailable" || data["automaticReplay"] != false {
				t.Fatalf("missing vision classification: %+v", rpc)
			}
			if withState {
				state := data["execution"].(map[string]any)
				if data["getMethod"] != getExecutionMethod || state["sessionId"] != "session" || state["runId"] != "run" || state["version"] != int64(7) || state["status"] != harness.ExecutionNeedsReconciliation || state["resumable"] != false {
					t.Fatalf("lost execution recovery state: %+v", data)
				}
			}
			encoded, marshalErr := json.Marshal(rpc)
			if marshalErr != nil || strings.Contains(string(encoded), "private-") {
				t.Fatalf("private diagnostics escaped: %s %v", encoded, marshalErr)
			}
		}
	}
}

func TestModelVisionFailureDoesNotHideRecoveryErrors(t *testing.T) {
	for _, recovery := range []error{harness.ErrReconciliationRequired, harness.ErrReceiptConflict, harness.ErrExecutionUnresumable, harness.ErrPermissionDenied} {
		want := executionUpdateError(recovery)
		got := executionUpdateError(errors.Join(harness.ErrModelVisionUnavailable, recovery))
		if got.Code != want.Code || got.Message != want.Message {
			t.Fatalf("vision guidance hid recovery error: got=%+v want=%+v", got, want)
		}
	}
	if got := executionUpdateError(fmt.Errorf("other media failure: %w", harness.ErrInvalidInput)); got.Message != "Invalid request parameters" {
		t.Fatalf("unrelated invalid input was classified as a vision failure: %+v", got)
	}
}
