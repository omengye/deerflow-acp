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

func TestCheckpointFailuresPreservePublicStateWithoutPrivateDetails(t *testing.T) {
	for _, item := range []struct {
		kind  string
		code  int
		cause error
	}{
		{"checkpoint_too_large", -32028, &harness.CheckpointSizeError{Representation: "raw", Bytes: 80 << 20, Limit: 64 << 20}},
		{"checkpoint_invalid", -32029, fmt.Errorf("%w: private-state fixture-secret", harness.ErrCheckpointInvalid)},
		{"checkpoint_persist_failed", -32030, errors.New("private-database fixture-secret")},
	} {
		t.Run(item.kind, func(t *testing.T) {
			cause := &harness.CheckpointCommitError{Cause: item.cause}
			var a Agent
			_, failure := a.runResponse(func(harness.EventHandler) (harness.RunResult, error) {
				return harness.RunResult{Execution: &harness.ExecutionState{SessionID: "session", RunID: "run", Status: harness.ExecutionFailed, Version: 9, BlockedReason: "private-checkpoint"}}, cause
			})
			if !errors.Is(failure, item.cause) {
				t.Fatal("original checkpoint cause was lost")
			}
			for _, public := range []error{mapError(failure), executionUpdateError(failure)} {
				var rpc *protocol.Error
				if !errors.As(public, &rpc) || rpc.Code != item.code {
					t.Fatalf("checkpoint classification=%+v", public)
				}
				encoded, err := json.Marshal(rpc)
				if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "fixture-secret") || strings.Contains(rpc.Message, "Invalid request parameters") {
					t.Fatalf("unsafe checkpoint diagnostic: %s %v", encoded, err)
				}
				data := rpc.Data.(map[string]any)
				state := data["execution"].(map[string]any)
				if data["kind"] != item.kind || data["automaticReplay"] != false || state["runId"] != "run" || state["resumable"] != false {
					t.Fatalf("checkpoint failure lost safe state: %+v", data)
				}
				if item.kind == "checkpoint_too_large" && (data["bytes"] != uint64(80<<20) || data["limit"] != uint64(64<<20)) {
					t.Fatalf("checkpoint size diagnostic=%+v", data)
				}
			}
		})
	}
}
