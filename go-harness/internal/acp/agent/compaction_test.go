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

func TestCompactionErrorMappingKeepsSafeDiagnostics(t *testing.T) {
	for _, reason := range []string{harness.CompactionTruncated, harness.CompactionEmpty, harness.CompactionOversized, harness.CompactionInvalid} {
		t.Run(reason, func(t *testing.T) {
			failure := &harness.CompactionError{Reason: reason, FinishReason: "length", SummaryBytes: 327, Attempts: 2}
			var rpc *protocol.Error
			if !errors.As(mapError(fmt.Errorf("private-wrapper: %w", failure)), &rpc) || rpc.Code != compactionErrorCode {
				t.Fatalf("compaction lost its client classification: %+v", rpc)
			}
			data := rpc.Data.(map[string]any)
			if data["kind"] != "context_compaction" || data["reason"] != reason || data["finishReason"] != "length" || data["summaryBytes"] != 327 || data["attempts"] != 2 || data["automaticReplay"] != false {
				t.Fatalf("missing safe metadata: %+v", data)
			}
			if strings.Contains(rpc.Message, "private-wrapper") || !strings.Contains(rpc.Message, "original conversation was preserved") {
				t.Fatalf("unsafe or unhelpful message: %s", rpc.Message)
			}
		})
	}
}

func TestCompactionErrorMappingSanitizesUnknownValues(t *testing.T) {
	failure := &harness.CompactionError{Reason: "private-summary", FinishReason: "secret-api-key", SummaryBytes: -1, Attempts: -2}
	var rpc *protocol.Error
	if !errors.As(mapError(failure), &rpc) {
		t.Fatal("missing RPC error")
	}
	data := rpc.Data.(map[string]any)
	if data["reason"] != harness.CompactionInvalid || data["finishReason"] != "unknown" || data["summaryBytes"] != 0 || data["attempts"] != 1 {
		t.Fatalf("unbounded diagnostics: %+v", data)
	}
	encoded, err := json.Marshal(rpc)
	if err != nil || strings.Contains(string(encoded), "private-summary") || strings.Contains(string(encoded), "secret-api-key") {
		t.Fatalf("raw diagnostics escaped: %s, %v", encoded, err)
	}
}

func TestCompactionErrorDoesNotHideUncertainEffects(t *testing.T) {
	failure := errors.Join(&harness.CompactionError{Reason: harness.CompactionTruncated}, harness.ErrReconciliationRequired)
	var rpc *protocol.Error
	if !errors.As(mapError(failure), &rpc) || rpc.Code != -32010 || rpc.Data.(map[string]any)["kind"] != "reconciliation_required" {
		t.Fatalf("summary guidance hid uncertain tool effects: %+v", rpc)
	}
}

func TestRunErrorPreservesSafeExecutionMetadataAndOriginalCause(t *testing.T) {
	for _, resumable := range []bool{false, true} {
		t.Run(fmt.Sprint(resumable), func(t *testing.T) {
			state := &harness.ExecutionState{
				SessionID: "session", RunID: "run", Version: 7, Resumable: true,
				Status: harness.ExecutionNeedsReconciliation, BlockedReason: "private-checkpoint-path",
				WaitingInputs: []harness.ExecutionInteraction{{ArgumentsSummary: "private-tool-input"}},
			}
			if resumable {
				state.Status = harness.ExecutionWaitingInput
			}
			failure := &harness.CompactionError{Reason: harness.CompactionTruncated, Attempts: 2}
			var a Agent
			value, err := a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
				return harness.RunResult{Execution: state}, errors.Join(failure, harness.ErrReconciliationRequired)
			})
			var retained *harness.CompactionError
			if value != nil || !errors.As(err, &retained) || retained != failure || !errors.Is(err, harness.ErrReconciliationRequired) {
				t.Fatalf("runResponse discarded the original error chain: %v", err)
			}
			var rpc *protocol.Error
			if !errors.As(mapError(err), &rpc) || rpc.Code != -32010 {
				t.Fatalf("recovery error lost its priority: %+v", rpc)
			}
			data := rpc.Data.(map[string]any)
			execution := data["execution"].(map[string]any)
			if data["getMethod"] != getExecutionMethod || data["automaticReplay"] != false || execution["runId"] != "run" || execution["sessionId"] != "session" || execution["version"] != int64(7) || execution["resumable"] != resumable {
				t.Fatalf("missing recovery identity: %+v", data)
			}
			encoded, marshalErr := json.Marshal(rpc)
			if marshalErr != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "waitingInputs") || strings.Contains(string(encoded), "blockedReason") {
				t.Fatalf("private execution state escaped: %s, %v", encoded, marshalErr)
			}
		})
	}
}

func TestExecutionErrorMetadataPreservesInternalErrorBoundary(t *testing.T) {
	var a Agent
	_, err := a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
		return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionFailed, Version: 2}}, errors.New("private-provider-error")
	})
	var rpc *protocol.Error
	if !errors.As(mapError(err), &rpc) || rpc.Code != protocol.InternalError || rpc.Message != "Internal error" {
		t.Fatalf("unknown error leaked or lost execution identity: %+v", rpc)
	}
	if rpc.Data.(map[string]any)["execution"] == nil {
		t.Fatal("missing failed execution identity")
	}
	original := &protocol.Error{Code: compactionErrorCode, Message: "safe", Data: map[string]any{"kind": "context_compaction"}}
	_, err = a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
		return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionFailed}}, original
	})
	_ = mapError(err)
	if original.Data.(map[string]any)["execution"] != nil {
		t.Fatal("execution enrichment mutated a shared protocol error")
	}
}
