package harness

import (
	"errors"
	"fmt"
	"testing"
)

func TestToolFailureEvidencePreservesErrorChain(t *testing.T) {
	cause := errors.New("fixture rejection")
	for _, test := range []struct {
		name string
		mark func(error) error
	}{
		{"not_executed", MarkToolNotExecuted},
		{"no_effect", MarkToolNoEffect},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.mark(nil) != nil {
				t.Fatal("nil error became a failure")
			}
			err := fmt.Errorf("adapter: %w", test.mark(cause))
			if !errors.Is(err, cause) {
				t.Fatal("original error lost")
			}
			var rejected *ToolNotExecutedError
			var readOnly *ToolNoEffectError
			if (test.name == "not_executed") != errors.As(err, &rejected) || (test.name == "no_effect") != errors.As(err, &readOnly) {
				t.Fatalf("wrong evidence: %v", err)
			}
		})
	}
}
