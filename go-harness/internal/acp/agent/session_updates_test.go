package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestAutomaticExecutionNotificationErrorsArePublic(t *testing.T) {
	rpc := executionUpdateError(errors.New("provider credential=fixture-secret in private/workspace"))
	encoded, err := json.Marshal(rpc)
	if err != nil || rpc.Code != protocol.InternalError || rpc.Message != "Internal error" || rpc.Data != nil || strings.Contains(string(encoded), "fixture-secret") {
		t.Fatalf("raw internal error exposed: %s %v", encoded, err)
	}
	rpc = executionUpdateError(harness.ErrExecutionUnresumable)
	if rpc.Code != executionUnresumableCode || rpc.Data == nil {
		t.Fatalf("lost public recovery guidance: %+v", rpc)
	}
}

func TestPinnedSearchConfigurationErrorReportsOnlyConfirmedExecutionStatus(t *testing.T) {
	cause := errors.Join(harness.ErrPinnedSearchConfigInvalid, errors.New("fixture-secret in private/workspace"))
	for _, status := range []harness.ExecutionStatus{"", harness.ExecutionWaitingInput, harness.ExecutionFailed} {
		var failure error = cause
		if status != "" {
			failure = &executionResultError{cause: cause, state: &harness.ExecutionState{
				SessionID: "session", RunID: "run", Status: status, Version: 11,
				BlockedReason: "fixture-secret",
			}}
		}
		rpc := executionUpdateError(failure)
		encoded, err := json.Marshal(rpc)
		if err != nil || rpc.Code != executionUnresumableCode || rpc.Data.(map[string]any)["kind"] != "pinned_search_config_invalid" ||
			!strings.Contains(rpc.Message, "搜索配置") || strings.Contains(rpc.Message, "本轮已停止") != (status == harness.ExecutionFailed) ||
			strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "private/workspace") {
			t.Fatalf("incorrect pinned search error for %s: %s, %v", status, encoded, err)
		}
	}
}

func TestContextLimitErrorsArePublicWithoutProviderDetails(t *testing.T) {
	err := errors.Join(harness.ErrContextLimit, errors.New("provider credential=fixture-secret in private/workspace"))
	for _, public := range []error{mapError(err), executionUpdateError(err)} {
		var rpc *protocol.Error
		if !errors.As(public, &rpc) {
			t.Fatalf("missing public error: %v", public)
		}
		encoded, marshalErr := json.Marshal(rpc)
		if marshalErr != nil || rpc.Code != -32024 || rpc.Message == "Internal error" || strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "private/workspace") {
			t.Fatalf("unsafe context diagnostic: %s %v", encoded, marshalErr)
		}
		data, ok := rpc.Data.(map[string]any)
		if !ok || data["kind"] != "context_limit" || data["automaticReplay"] != false {
			t.Fatalf("missing context recovery guidance: %+v", rpc)
		}
	}
	// A context error never overrides an outstanding tool reconciliation gate.
	rpc := executionUpdateError(errors.Join(harness.ErrContextLimit, harness.ErrReconciliationRequired))
	if rpc.Code == -32024 {
		t.Fatal("context error hid unresolved tool effects")
	}
}

func TestMCPResourceErrorsArePublicAndDoNotExposePrivateDetails(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		code  int
	}{
		{"incompatible", errors.Join(harness.ErrResourcesWaiting, harness.ErrResourcesIncompatible), protocol.InvalidParams},
		{"waiting", harness.ErrResourcesWaiting, protocol.ServerBusy},
		{"permission", harness.ErrPermissionDenied, protocol.InvalidParams},
	} {
		t.Run(test.name, func(t *testing.T) {
			rpc := executionUpdateError(errors.Join(test.cause, errors.New("fixture-secret in private/workspace")))
			encoded, err := json.Marshal(rpc)
			if err != nil || rpc.Code != test.code || rpc.Message == "Internal error" || !strings.Contains(rpc.Message, "MCP") || strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "private/workspace") {
				t.Fatalf("invalid public MCP diagnostic: %s %v", encoded, err)
			}
		})
	}
}
