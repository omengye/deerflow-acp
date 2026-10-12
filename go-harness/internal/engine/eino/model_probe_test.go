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

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestAgentProbeBoundedRequestsAndCapabilities(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if failed {
					http.Error(w, "private-key-and-provider-details", 503)
					return
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request["tools"] == nil {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"id\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\n")
					fmt.Fprint(w, "data: {\"id\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
				} else {
					_ = modelstreamtest.WriteJSON(w, `{"id":"probe","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"deerflow_probe","arguments":"{\"marker\":\"deerflow-ok\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := ProbeAgentCapabilities(ctx, "openai", "fixture", server.URL, "private-key")
			if err != nil || calls.Load() != 2 || result.Requests != 2 || result.Streaming.OK == failed || result.Tools.OK == failed {
				t.Fatalf("probe=%+v calls=%d err=%v", result, calls.Load(), err)
			}
			if !failed && !result.Usage {
				t.Fatal("provider usage lost")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := ProbeAgentCapabilities(ctx, "openai", "fixture", "http://127.0.0.1:1", "key")
	if err != nil || result.Requests != 1 || result.Tools.Code != "cancelled" {
		t.Fatalf("cancelled=%+v err=%v", result, err)
	}
}

func TestClassifiedFailureDoesNotHideCancellationOrEffects(t *testing.T) {
	network := harness.ClassifiedToolFailure("network", "sanitized network failure")
	if toolFailureCode(network) != "network" || toolFailureCode(errors.Join(network, context.Canceled)) != "cancelled" || certifiedToolOutcome(network) {
		t.Fatal("classification changed cancellation/effect evidence")
	}
}
