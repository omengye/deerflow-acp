package harness

import "errors"

// ErrModelRateLimited classifies a rate limit reported by the model provider.
// It does not authorize replay of a model call or completed tools.
var ErrModelRateLimited = errors.New("model provider rate limit exceeded")
