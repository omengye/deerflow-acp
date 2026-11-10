package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chatapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/openai/openai-go/v3/responses"
)

func responseAdapterFixture(t *testing.T, handler http.HandlerFunc) model.ToolCallingChatModel {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m, err := newResponsesModel(context.Background(), Config{BaseURL: server.URL + "/v1", APIKey: "fixture-key"}, "fixture", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func writeResponseAdapterEvent(w http.ResponseWriter, event any) {
	encoded, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestResponsesAdapterOpaqueHistorySurvivesSQLite(t *testing.T) {
	const output = `[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-private-reasoning"},{"type":"function_call","id":"fc_item","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"one\"}","status":"completed"}]`
	var response responses.Response
	if err := json.Unmarshal([]byte(`{"output":`+output+`,"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"output_tokens_details":{"reasoning_tokens":2}}}`), &response); err != nil {
		t.Fatal(err)
	}
	message, err := responsesOutput(response, false)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "" || message.ReasoningContent != "" || message.ToolCalls[0].ID != "call_1" {
		t.Fatal("private output leaked or tool identity changed")
	}
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AppendEvents(context.Background(), "session", []*adk.SessionEvent[*schema.Message]{{EventID: "turn", Kind: adk.SessionEventMessage, Message: message}}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := store.LoadEvents(context.Background(), "session", nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded := history.Events[0].Message
	items, err := responsesInput([]*schema.Message{loaded, schema.ToolMessage("result", "call_1")})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "opaque-private-reasoning") || !strings.Contains(string(encoded), `"id":"fc_item"`) || !strings.Contains(string(encoded), `"call_id":"call_1"`) {
		t.Fatal("opaque provider history was lost after restart")
	}
	loaded.ToolCalls[0].Function.Arguments = `{"q":"changed"}`
	items, err = responsesInput([]*schema.Message{loaded})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(items)
	if !strings.Contains(string(encoded), "changed") || !strings.Contains(string(encoded), `"id":"fc_item"`) || strings.Contains(string(encoded), `\"one\"`) {
		t.Fatal("host reduction did not replace original tool arguments")
	}
	loaded.ToolCalls = nil
	items, err = responsesInput([]*schema.Message{loaded})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(items)
	if strings.Contains(string(encoded), "function_call") {
		t.Fatal("removed tool call was replayed from opaque history")
	}
	loaded.Extra[responsesOriginKey] = "old-endpoint"
	items, err = responsesInputForOrigin([]*schema.Message{loaded}, "new-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(items)
	if strings.Contains(string(encoded), "opaque-private-reasoning") || strings.Contains(string(encoded), "rs_1") {
		t.Fatal("provider-private reasoning was sent to a different endpoint")
	}
}

func TestResponsesAdapterPreservesEditedAnswerAndMultimodalToolOutput(t *testing.T) {
	var response responses.Response
	if err := json.Unmarshal([]byte(`{"output":[{"type":"reasoning","id":"rs","encrypted_content":"opaque","summary":[]},{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"tail"}]}]}`), &response); err != nil {
		t.Fatal(err)
	}
	message, err := responsesOutput(response, false)
	if err != nil {
		t.Fatal(err)
	}
	message.Content = "prefix tail"
	data := "aW1hZ2U="
	tool := &schema.Message{Role: schema.Tool, ToolCallID: "call", UserInputMultiContent: []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: "screenshot"}, {Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", Base64Data: &data}}}}}
	items, err := responsesInput([]*schema.Message{message, tool})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(items)
	text := string(encoded)
	if !strings.Contains(text, "prefix tail") || strings.Contains(text, `"text":"tail"`) || !strings.Contains(text, "encrypted_content") || !strings.Contains(text, `"output":[`) || !strings.Contains(text, "data:image/png;base64,"+data) {
		t.Fatal("edited history or native multimodal tool output was not preserved")
	}
}

func TestResponsesAdapterRejectsMissingTerminalAndLateFailure(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeResponseAdapterEvent(w, map[string]any{"type": "response.output_text.delta", "delta": "partial"})
				if terminal {
					writeResponseAdapterEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "partial"}}}}}})
					writeResponseAdapterEvent(w, map[string]any{"type": "error", "code": "server_error", "message": "late failure"})
				}
			})
			result, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
			if result != nil || !errors.Is(err, harness.ErrModelResponseInvalid) {
				t.Fatalf("incomplete stream accepted: %v", err)
			}
		})
	}
}

func TestResponsesAdapterReaderCloseCancelsIdleHTTP(t *testing.T) {
	closed := make(chan struct{})
	m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	})
	stream, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the reader left an idle HTTP request running")
	}
}

func TestResponsesAdapterContextCancellationRetainsCause(t *testing.T) {
	m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	for range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := m.Stream(ctx, []*schema.Message{schema.UserMessage("hello")})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		_, err = stream.Recv()
		stream.Close()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation misclassified: %v", err)
		}
	}
}

func TestResponsesAdapterOptionsAndIncompleteUsage(t *testing.T) {
	m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["store"] != false || body["max_output_tokens"] != float64(77) || body["reasoning"].(map[string]any)["effort"] != "high" || body["max_completion_tokens"] != nil || body["stream"] != true {
			t.Errorf("unexpected Responses options: %v", body)
		}
		tools := body["tools"].([]any)
		if tools[0].(map[string]any)["strict"] != false {
			t.Error("existing schema was made strict")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponseAdapterEvent(w, map[string]any{"type": "response.incomplete", "response": map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{map[string]any{"type": "function_call", "call_id": "call", "name": "lookup", "arguments": "{"}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 77, "total_tokens": 87, "input_tokens_details": map[string]any{"cached_tokens": 2}, "output_tokens_details": map[string]any{"reasoning_tokens": 50}}}})
	})
	m, err := m.WithTools([]*schema.ToolInfo{{Name: "lookup"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")}, model.WithMaxTokens(77), withResponsesTuning("high", nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.ResponseMeta.FinishReason != "length" || result.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens != 50 || result.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 2 || len(result.ToolCalls) != 1 {
		t.Fatal("incomplete response lost output-limit guard or usage")
	}
}

func TestResponsesAdapterHTTPErrorRetainsClassificationWithoutRequest(t *testing.T) {
	m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`)
	})
	stream, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	var api *chatapi.APIError
	if stream != nil || !errors.As(err, &api) || api.HTTPStatusCode != 429 || strings.Contains(err.Error(), "fixture-key") {
		t.Fatalf("unexpected provider error: %v", err)
	}
}

func TestResponsesAdapterDistinguishesMissingUsageFromZero(t *testing.T) {
	for _, test := range []struct {
		body    string
		present bool
	}{{`{}`, false}, {`{"usage":null}`, false}, {`{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`, true}} {
		var response responses.Response
		if err := json.Unmarshal([]byte(test.body), &response); err != nil {
			t.Fatal(err)
		}
		message, err := responsesOutput(response, false)
		if err != nil {
			t.Fatal(err)
		}
		if (message.ResponseMeta.Usage != nil) != test.present {
			t.Fatalf("usage presence lost for %s", test.body)
		}
	}
}
