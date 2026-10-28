package harness

import "errors"

// ErrModelResponseInvalid classifies a provider response that could not be
// decoded. It does not authorize replay of a model call or completed tools.
var ErrModelResponseInvalid = errors.New("model response could not be decoded")
