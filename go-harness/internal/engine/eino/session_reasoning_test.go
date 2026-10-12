package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSessionReasoningModelWire(t *testing.T) {
	for _, provider := range []string{"openai", "openai-compatible"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if call == 1 && body["reasoning_effort"] != "high" || call == 2 && body["reasoning_effort"] != nil {
					t.Errorf("call %d effort=%v", call, body["reasoning_effort"])
				}
				if call == 1 && (body["max_completion_tokens"] != float64(256) || body["max_tokens"] != nil) {
					t.Errorf("budget bypass: %v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			e := newTestEngine(t, Config{Provider: provider, Model: "fixture", APIKey: "fixture", BaseURL: server.URL, ModelReasoningEfforts: map[string][]string{"fixture": {"low", "high"}}, Budget: harness.BudgetLimits{MaxOutputTokens: 256}})
			for i, effort := range []string{"high", "", "xhigh", "max"} {
				req := request(fmt.Sprintf("effort-%d", i))
				req.Session.ID = fmt.Sprintf("session-%d", i)
				req.Session.Model, req.Session.ReasoningEffort = "fixture", effort
				result, err := e.Run(context.Background(), req, nil, nil)
				if i < 2 && (err != nil || result.StopReason != "end_turn") {
					t.Fatalf("result=%+v error=%v", result, err)
				}
				if i == 3 && err == nil {
					t.Fatal("max effort should remain unavailable until the model capability list includes it")
				}
				if i == 2 && err == nil {
					t.Fatal("unadvertised effort was accepted")
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("provider calls=%d", calls.Load())
			}
		})
	}
}

func TestSessionReasoningResumeIdentity(t *testing.T) {
	req := request("identity")
	original := itemFor(req)
	req.Session.ReasoningEffort = "high"
	if original == itemFor(req) {
		t.Fatal("an interrupted turn must not accept a different effort")
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, ok := legacy["ReasoningEffort"]; ok {
		t.Fatal("default effort changed legacy checkpoint encoding")
	}
}
