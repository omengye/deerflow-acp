package harness

import "errors"

// ErrModelToolsRequireResponses identifies a provider rejection of reasoning
// with function tools through Chat Completions. It never authorizes replay.
var ErrModelToolsRequireResponses = errors.New("model function tools require the Responses API")
