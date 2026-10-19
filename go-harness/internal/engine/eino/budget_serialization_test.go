package eino

import (
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestBudgetErrorSurvivesNativeEventCheckpoint(t *testing.T) {
	for _, resource := range []string{"model_calls", "tool_calls", "tokens", "time"} {
		t.Run(resource, func(t *testing.T) {
			serializer := &schema.GobSerializer{}
			original := &budgetError{resource: resource}
			data, err := serializer.Marshal(&adk.AgentEvent{Err: original})
			if err != nil {
				t.Fatal("budget stop cannot be checkpointed", err)
			}
			var restored adk.AgentEvent
			if err := serializer.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			var limit *budgetError
			if !errors.As(restored.Err, &limit) || limit.resource != resource || limit.stopReason() != original.stopReason() || limit.Error() != original.Error() {
				t.Fatalf("checkpoint lost budget termination: %#v", restored.Err)
			}
		})
	}
}

func TestBudgetErrorCheckpointRejectsUnknownResource(t *testing.T) {
	for _, resource := range []string{"", "other", "model_calls\x00"} {
		var limit budgetError
		if err := limit.GobDecode([]byte(resource)); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("unknown resource %q accepted: %v", resource, err)
		}
	}
}
