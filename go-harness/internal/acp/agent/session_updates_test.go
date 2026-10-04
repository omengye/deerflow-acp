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
