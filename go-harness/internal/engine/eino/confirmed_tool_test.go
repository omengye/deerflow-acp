package eino

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSafePreparationErrorSkillEditArguments(t *testing.T) {
	for _, test := range []struct{ reason, want string }{
		{"reference_name", "Remove name"},
		{"reference_edits", "Remove edits"},
		{"reference_edit_id", "requires edit_id"},
		{"prepare_edit_id", "omit edit_id"},
		{"json", "one JSON object"},
		{"action", "must be prepare, apply or status"},
		{"PRIVATE_ARGUMENT_VALUE", "must be prepare, apply or status"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			err := fmt.Errorf("PRIVATE_ARGUMENT_VALUE: %w", &harness.SkillEditArgumentError{Reason: test.reason})
			message := safePreparationError(err)
			if !errors.Is(err, harness.ErrInvalidInput) || !strings.Contains(message, test.want) || strings.Contains(message, "PRIVATE_ARGUMENT_VALUE") {
				t.Fatalf("unsafe or unhelpful argument error: %s", message)
			}
		})
	}
	// Do not expose arbitrary invalid-input details from other validation paths.
	message := safePreparationError(fmt.Errorf("%w: PRIVATE_ARGUMENT_VALUE", harness.ErrInvalidInput))
	if strings.Contains(message, "PRIVATE_ARGUMENT_VALUE") {
		t.Fatal("unclassified validation error leaked its payload")
	}
}
