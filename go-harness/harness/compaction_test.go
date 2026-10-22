package harness

import (
	"strings"
	"testing"
)

func TestCompactionErrorHasSafeActionableMessage(t *testing.T) {
	for _, test := range []struct {
		reason string
		want   string
	}{
		{CompactionTruncated, "output limit"},
		{CompactionEmpty, "empty summary"},
		{CompactionOversized, "safe size limit"},
		{CompactionInvalid, "summary was invalid"},
		{"private-model-output", "summary was invalid"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			err := &CompactionError{Reason: test.reason, FinishReason: "secret-provider-detail", SummaryBytes: 17000, Attempts: 2}
			message := err.Error()
			if !strings.Contains(message, test.want) || !strings.Contains(message, "original conversation was preserved") || !strings.Contains(message, "completed tool results") {
				t.Fatalf("missing safe recovery guidance: %s", message)
			}
			if strings.Contains(message, "private-model-output") || strings.Contains(message, "secret-provider-detail") {
				t.Fatalf("raw diagnostics leaked: %s", message)
			}
		})
	}
	var missing *CompactionError
	if !strings.Contains(missing.Error(), "summary was invalid") {
		t.Fatal("nil diagnostic must have a safe message")
	}
}
