package harness

import "fmt"

// ErrModelVisionUnavailable means the selected model has no configured vision
// capability for images in the input, tool results, or retained conversation.
// It remains invalid input for callers that do not inspect this finer cause.
var ErrModelVisionUnavailable = fmt.Errorf("%w: selected model does not have configured vision capability", ErrInvalidInput)
