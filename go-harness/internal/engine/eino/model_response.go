package eino

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Only the model boundary calls this classifier; tool argument and storage
// JSON errors must not be presented as invalid provider responses.
func classifyModelResponseError(err error) error {
	var syntax *json.SyntaxError
	if errors.Is(err, harness.ErrModelResponseInvalid) || !errors.As(err, &syntax) {
		return err
	}
	return fmt.Errorf("%w: %w", harness.ErrModelResponseInvalid, err)
}
