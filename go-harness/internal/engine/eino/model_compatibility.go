package eino

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Recognize this specific provider response only at the model boundary. Plain
// error text, tools and unrelated HTTP 400 responses cannot establish it.
func classifyModelToolsCompatibilityError(err error) error {
	if errors.Is(err, harness.ErrModelToolsRequireResponses) {
		return err
	}
	_, found := removeErrorLeaves(err, func(leaf error) bool {
		api, ok := leaf.(*openaiapi.APIError)
		if !ok || api.HTTPStatusCode != http.StatusBadRequest {
			return false
		}
		message := strings.ToLower(strings.TrimSpace(api.Message))
		return strings.HasPrefix(message, "function tools with reasoning_effort are not supported") &&
			strings.Contains(message, "/v1/chat/completions") &&
			strings.Contains(message, "/v1/responses")
	})
	if !found {
		return err
	}
	return fmt.Errorf("%w: %w", harness.ErrModelToolsRequireResponses, err)
}
