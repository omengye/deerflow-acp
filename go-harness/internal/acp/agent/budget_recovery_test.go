package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func TestUnsettledBudgetReturnsPublicExecutionRecovery(t *testing.T) {
	err := errors.Join(budget.ErrUnknown, &budget.PersistenceError{Operation: "settle", Err: errors.New("fixture-secret in private/workspace")})
	rpc := executionUpdateError(err)
	encoded, marshalErr := json.Marshal(rpc)
	if marshalErr != nil || rpc.Code != executionUnresumableCode || rpc.Data == nil || !strings.Contains(rpc.Message, "Read its state") {
		t.Fatalf("missing public recovery guidance: %s %v", encoded, marshalErr)
	}
	if strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "private/workspace") {
		t.Fatalf("private settlement diagnostic exposed: %s", encoded)
	}
}
