package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestResponsesAdapterAcceptsEmptySSEFramesWithoutRetry(t *testing.T) {
	for name, frame := range map[string]string{
		"comment_heartbeat":  ": keep-alive\n\n",
		"blank_separator":    "\n\n",
		"empty_data":         "data:\n\n",
		"whitespace_data":    "data:  \t\r\n\r\n",
		"event_without_data": "event: response.output_text.delta\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				fmt.Fprint(w, frame)
				writeResponseAdapterEvent(w, map[string]any{"type": "response.output_text.delta", "delta": "OK"})
				fmt.Fprint(w, frame)
				// SSE permits JSON to span multiple data lines within one event.
				fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\ndata: \"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"total_tokens\":6}}}\n\n")
				fmt.Fprint(w, frame)
			})
			message, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("fixture")})
			if err != nil || message == nil || message.Content != "OK" || message.ResponseMeta.FinishReason != "stop" || message.ResponseMeta.Usage.TotalTokens != 6 || requests.Load() != 1 {
				t.Fatalf("valid SSE framing failed: message=%+v err=%v requests=%d", message, err, requests.Load())
			}
		})
	}
}

func TestResponsesAdapterStillRejectsNonemptyMalformedSSE(t *testing.T) {
	for _, broken := range []string{`{"type":"response.output_text.delta",`, `not-json`} {
		t.Run(broken, func(t *testing.T) {
			var requests atomic.Int32
			m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				writeResponseAdapterEvent(w, map[string]any{"type": "response.output_text.delta", "delta": "partial"})
				fmt.Fprintf(w, "data: %s\n\n", broken)
			})
			stream, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("fixture")})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var text strings.Builder
			for {
				var chunk *schema.Message
				chunk, err = stream.Recv()
				if err != nil {
					break
				}
				text.WriteString(chunk.Content)
			}
			var syntax *json.SyntaxError
			if !errors.As(err, &syntax) || text.String() != "partial" || requests.Load() != 1 {
				t.Fatalf("malformed payload was hidden or replayed: err=%v text=%q requests=%d", err, text.String(), requests.Load())
			}
		})
	}
}
