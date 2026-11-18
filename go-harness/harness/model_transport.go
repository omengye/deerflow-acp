package harness

import "errors"

// ErrModelUnavailable classifies transient model/provider transport failures.
// It does not authorize replay of an execution or its completed tool calls.
var ErrModelUnavailable = errors.New("model provider connection unavailable")
