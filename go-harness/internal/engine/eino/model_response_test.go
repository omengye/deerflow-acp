package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	segmentjson "github.com/segmentio/encoding/json"
)

func TestProviderModelResponseClassificationPreservesCause(t *testing.T) {
	for name, decode := range map[string]func([]byte, any) error{"standard": json.Unmarshal, "segmentio": segmentjson.Unmarshal} {
		t.Run(name, func(t *testing.T) {
			var value any
			cause := decode([]byte(`{"content":"private-provider-content"`), &value)
			var syntax *json.SyntaxError
			if !errors.As(cause, &syntax) {
				t.Fatalf("provider decoder did not preserve standard syntax error: %T", cause)
			}
			for _, role := range []bool{false, true} {
				wrapped := fmt.Errorf("private-provider-diagnostic: %w", cause)
				got := (&trackedModel{role: role}).modelError(wrapped)
				var retained *json.SyntaxError
				if !errors.Is(got, harness.ErrModelResponseInvalid) || !errors.Is(got, wrapped) || !errors.Is(got, cause) || !errors.As(got, &retained) || retained != syntax {
					t.Fatalf("role=%t lost classification or original cause: %v", role, got)
				}
				if classifyModelResponseError(got) != got {
					t.Fatal("already-classified failure was wrapped again")
				}
			}
		})
	}
	for _, cause := range []error{nil, errors.New("unexpected end of JSON input"), context.Canceled, io.ErrUnexpectedEOF, &json.UnmarshalTypeError{Value: "string"}} {
		if got := classifyModelResponseError(cause); got != cause {
			t.Fatalf("unclassified error changed: %T: %v", cause, got)
		}
	}
}

func TestProviderMalformedStreamStopsWithoutReplay(t *testing.T) {
	for _, mode := range []string{"stream", "generate", "run"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial fixture output\"}}]}\n\n")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"private-provider-content\"\n\n")
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cfg := Config{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture-secret"}
			var failure error
			if mode == "run" {
				e := newTestEngine(t, cfg)
				_, failure = e.Run(ctx, request("invalid-provider-response"), nil, nil)
			} else {
				chat, err := newModel(ctx, cfg, cfg.Model)
				if err != nil {
					t.Fatal(err)
				}
				work := newRunIO(ctx)
				tracked := &trackedModel{inner: chat, io: work, budget: &runBudget{limits: harness.BudgetLimits{MaxModelCalls: 1, MaxTokens: 10000, MaxOutputTokens: 64}}, sink: &eventSink{}}
				input := []*schema.Message{schema.UserMessage("fixture")}
				if mode == "generate" {
					var answer *schema.Message
					answer, failure = tracked.Generate(ctx, input)
					if answer != nil {
						t.Fatalf("incomplete stream was returned as an answer: %+v", answer)
					}
				} else {
					stream, err := tracked.Stream(ctx, input)
					if err != nil {
						t.Fatal(err)
					}
					var partial strings.Builder
					for {
						chunk, err := stream.Recv()
						if err != nil {
							failure = err
							break
						}
						if chunk != nil {
							partial.WriteString(chunk.Content)
						}
					}
					stream.Close()
					if partial.String() != "partial fixture output" {
						t.Fatalf("failure did not follow partial output: %q", partial.String())
					}
				}
				work.closeAndWait()
			}
			var syntax *json.SyntaxError
			if !errors.Is(failure, harness.ErrModelResponseInvalid) || !errors.As(failure, &syntax) || calls.Load() != 1 {
				t.Fatalf("calls=%d error=%v", calls.Load(), failure)
			}
		})
	}
}
