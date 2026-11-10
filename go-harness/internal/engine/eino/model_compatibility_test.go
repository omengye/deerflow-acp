package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

const toolsCompatibilityMessage = "Function tools with reasoning_effort are not supported for fixture-model in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'."

func TestModelToolsCompatibilityRequiresTypedProviderRejection(t *testing.T) {
	cause := &openaiapi.APIError{HTTPStatusCode: http.StatusBadRequest, Message: toolsCompatibilityMessage}
	wrapped := fmt.Errorf("provider: %w", cause)
	got := (&trackedModel{}).modelError(wrapped)
	if !errors.Is(got, harness.ErrModelToolsRequireResponses) || !errors.Is(got, cause) || retryableModelRateLimit(got) {
		t.Fatal("provider incompatibility lost its classification or cause, or authorized replay")
	}
	if classifyModelToolsCompatibilityError(got) != got {
		t.Fatal("compatibility classification was not idempotent")
	}
	for _, other := range []error{
		nil, errors.New(toolsCompatibilityMessage),
		&openaiapi.APIError{HTTPStatusCode: http.StatusUnauthorized, Message: toolsCompatibilityMessage},
		&openaiapi.APIError{HTTPStatusCode: http.StatusBadRequest, Message: "invalid tool schema"},
		&openaiapi.APIError{HTTPStatusCode: http.StatusBadRequest, Message: "reasoning_effort must be supported"},
		&openaiapi.APIError{HTTPStatusCode: http.StatusBadRequest, Message: "Function tools with reasoning_effort are not supported for fixture-model in /v1/chat/completions."},
		&budget.PersistenceError{Operation: "settle", Err: cause},
	} {
		if classified := classifyModelToolsCompatibilityError(other); classified != other {
			t.Fatalf("unrelated failure was classified: %T", other)
		}
	}
}

func TestModelToolsCompatibilityClassifiesActualProviderHTTPResponse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": toolsCompatibilityMessage, "type": "invalid_request_error"}})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chat, err := newModel(ctx, Config{Provider: "openai-compatible", BaseURL: server.URL, APIKey: "fixture-secret"}, "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := chat.Stream(ctx, []*schema.Message{schema.UserMessage("fixture")})
	if stream != nil {
		stream.Close()
	}
	var provider *openaiapi.APIError
	if !errors.Is(err, harness.ErrModelToolsRequireResponses) || !errors.As(err, &provider) || calls.Load() != 1 {
		t.Fatalf("HTTP rejection was not classified without replay: %v, calls=%d", err, calls.Load())
	}
}
