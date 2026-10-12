// Package modelstreamtest supplies stream-only loopback model fixtures.
package modelstreamtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// RequireStreaming rejects the same unsupported transport as stream-only
// providers, without changing the handler's prompt or route assertions.
func RequireStreaming(t *testing.T, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		var request struct{ Stream bool }
		if err != nil || json.Unmarshal(raw, &request) != nil || !request.Stream {
			t.Error("model request must use streaming transport")
			http.Error(w, `{"error":{"message":"Non-stream chat request is currently not supported"}}`, http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next(w, r)
	}
}

// WriteCompletion emits content/tool deltas, then a separate usage-only tail.
// Keeping the tail separate catches collectors that stop at finish_reason.
func WriteCompletion(w http.ResponseWriter, body map[string]any) error {
	w.Header().Set("Content-Type", "text/event-stream")
	choices, _ := body["choices"].([]any)
	for index, item := range choices {
		choice := item.(map[string]any)
		message := choice["message"].(map[string]any)
		if calls, ok := message["tool_calls"].([]map[string]any); ok {
			for i, call := range calls {
				call["index"] = i
			}
		} else if calls, ok := message["tool_calls"].([]any); ok {
			for i, call := range calls {
				call.(map[string]any)["index"] = i
			}
		}
		data, err := json.Marshal(map[string]any{"id": "stream-fixture", "choices": []any{map[string]any{"index": index, "delta": message, "finish_reason": choice["finish_reason"]}}})
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
	}
	if usage := body["usage"]; usage != nil {
		data, err := json.Marshal(map[string]any{"id": "stream-fixture", "choices": []any{}, "usage": usage})
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	return err
}

func WriteJSON(w http.ResponseWriter, raw string) error {
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return err
	}
	return WriteCompletion(w, body)
}
