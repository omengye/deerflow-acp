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

func TestModelRateLimitErrorsArePublicWithoutProviderDetails(t *testing.T) {
	cause := fmt.Errorf("%w: provider credential=fixture-secret in private/workspace", harness.ErrModelRateLimited)
	for _, notify := range []bool{false, true} {
		for _, withState := range []bool{false, true} {
			err := error(cause)
			if withState {
				var a Agent
				_, err = a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
					return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionFailed, Version: 14, BlockedReason: "private-checkpoint-path"}}, cause
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
			if marshalErr != nil || rpc.Code != -32027 || !strings.Contains(rpc.Message, "模型服务请求受到限流") || strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "private") {
				t.Fatalf("unsafe model rate limit diagnostic: %s %v", encoded, marshalErr)
			}
			data, ok := rpc.Data.(map[string]any)
			if !ok || data["kind"] != "model_rate_limited" || data["automaticReplay"] != false {
				t.Fatalf("missing model rate limit classification: %+v", rpc)
			}
			if withState {
				state := data["execution"].(map[string]any)
				if data["getMethod"] != getExecutionMethod || state["runId"] != "run" || state["sessionId"] != "session" || state["status"] != harness.ExecutionFailed || state["version"] != int64(14) || state["resumable"] != false {
					t.Fatalf("lost safe execution state: %+v", data)
				}
			}
		}
	}
}

func TestModelRateLimitDoesNotHideRecoveryErrors(t *testing.T) {
	for _, cause := range []error{harness.ErrReconciliationRequired, harness.ErrReceiptConflict, harness.ErrExecutionUnresumable, harness.ErrContextLimit, harness.ErrTaskTreeStopped, harness.ErrPermissionDenied, harness.ErrModelResponseInvalid, &harness.CompactionError{Reason: harness.CompactionTruncated}} {
		want := executionUpdateError(cause)
		got := executionUpdateError(errors.Join(harness.ErrModelRateLimited, cause))
		if got.Code != want.Code || got.Message != want.Message {
			t.Fatalf("rate limit classification hid a higher-priority error: got=%+v want=%+v", got, want)
		}
	}
	if rpc := executionUpdateError(errors.New("tool reported status code 429")); rpc.Code != protocol.InternalError {
		t.Fatalf("non-model error was classified as a model rate limit: %+v", rpc)
	}
}

func TestModelRateLimitDoesNotHidePersistenceFailure(t *testing.T) {
	cause := errors.Join(harness.ErrModelRateLimited, &budget.PersistenceError{Operation: "settle", Err: errors.New("private-database-path fixture-secret")})
	for _, withState := range []bool{false, true} {
		failure := cause
		if withState {
			var a Agent
			_, failure = a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
				return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionFailed, Version: 14}}, cause
			})
		}
		rpc := executionUpdateError(failure)
		encoded, err := json.Marshal(rpc)
		if err != nil || rpc.Code != protocol.InternalError || strings.Contains(string(encoded), "model_rate_limited") || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "fixture-secret") {
			t.Fatalf("rate limit hid persistence failure or exposed diagnostics: %s, %v", encoded, err)
		}
		if withState && rpc.Data.(map[string]any)["execution"] == nil {
			t.Fatal("persistence failure lost execution metadata")
		}
	}
}
