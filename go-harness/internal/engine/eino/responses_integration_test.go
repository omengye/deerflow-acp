package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func writeIntegrationResponseEvent(w http.ResponseWriter, event map[string]any) {
	body, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], body)
	w.(http.Flusher).Flush()
}

func TestResponsesSelectedRouteEngineToolRoundTripUsesProxy(t *testing.T) {
	const opaque = "OPAQUE_RESPONSES_REASONING_FIXTURE"
	const args = `{"value":"fixture"}`
	const answer = "The workspace check completed."
	var requests atomic.Int32
	underlying := &recordingTool{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Host != "responses.fixture.invalid" || r.URL.Path != "/v1/responses" {
			t.Errorf("selected route did not reach the proxy Responses endpoint: %s %s", r.Method, r.URL)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reasoning, _ := body["reasoning"].(map[string]any)
		if body["model"] != "responses-model" || body["stream"] != true || body["store"] != false || body["max_output_tokens"] != float64(128) || reasoning["effort"] != "high" {
			t.Errorf("route, streaming, reasoning or fixed output cap lost: model=%v stream=%v store=%v cap=%v reasoning=%v", body["model"], body["stream"], body["store"], body["max_output_tokens"], reasoning)
		}
		if body["max_completion_tokens"] != nil || body["max_tokens"] != nil || body["reasoning_effort"] != nil {
			t.Error("Chat Completions parameters leaked into the Responses request")
		}
		include, _ := body["include"].([]any)
		foundEncrypted := false
		for _, item := range include {
			foundEncrypted = foundEncrypted || item == "reasoning.encrypted_content"
		}
		if !foundEncrypted {
			t.Error("stateless Responses request did not request encrypted reasoning")
		}
		tools, _ := body["tools"].([]any)
		foundTool := false
		for _, raw := range tools {
			item, _ := raw.(map[string]any)
			foundTool = foundTool || item["type"] == "function" && item["name"] == "read_workspace"
		}
		if !foundTool {
			t.Error("native tool schema did not reach the selected Responses model")
		}
		var output []any
		inputTokens, outputTokens := 20, 4
		switch call {
		case 1:
			output = []any{
				map[string]any{"type": "reasoning", "id": "rs_fixture", "summary": []any{}, "encrypted_content": opaque},
				map[string]any{"type": "function_call", "id": "fc_fixture", "call_id": "call_fixture", "name": "read_workspace", "arguments": args, "status": "completed"},
			}
		case 2:
			input, _ := body["input"].([]any)
			reasoningAt, callAt, resultAt := -1, -1, -1
			for index, raw := range input {
				item, _ := raw.(map[string]any)
				switch item["type"] {
				case "reasoning":
					if item["id"] == "rs_fixture" && item["encrypted_content"] == opaque {
						reasoningAt = index
					}
				case "function_call":
					if item["id"] == "fc_fixture" && item["call_id"] == "call_fixture" && item["arguments"] == args {
						callAt = index
					}
				case "function_call_output":
					value, _ := item["output"].(string)
					if item["call_id"] == "call_fixture" && strings.Contains(value, "tool result: "+args) {
						resultAt = index
					}
				}
			}
			if reasoningAt < 0 || callAt <= reasoningAt || resultAt <= callAt || underlying.calls.Load() != 1 {
				t.Errorf("native tool roundtrip lost opaque reasoning/order/call identity: reasoning=%d call=%d result=%d executed=%d", reasoningAt, callAt, resultAt, underlying.calls.Load())
			}
			inputTokens, outputTokens = 30, 5
			output = []any{map[string]any{"type": "message", "id": "msg_fixture", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer, "annotations": []any{}}}}}
		default:
			t.Error("tool roundtrip issued an unexpected additional model request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\n\n")
		if call == 2 {
			writeIntegrationResponseEvent(w, map[string]any{"type": "response.output_text.delta", "sequence_number": 1, "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": answer})
			fmt.Fprint(w, "data:\n\n")
		}
		writeIntegrationResponseEvent(w, map[string]any{"type": "response.completed", "sequence_number": 2, "response": map[string]any{
			"id": fmt.Sprintf("resp_fixture_%d", call), "object": "response", "status": "completed", "output": output,
			"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens},
		}})
		fmt.Fprint(w, ": stream heartbeat\n\n")
	}))
	defer proxy.Close()
	policy := harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: true, URL: proxy.URL}}
	e := newTestEngine(t, Config{
		Provider: "openai", Model: "root", BaseURL: "http://root.fixture.invalid/v1", APIKey: "fixture-key", Network: policy,
		ModelRoutes:           map[string]harness.ModelRoute{"alternate": {Provider: "openai-compatible", Model: "responses-model", API: harness.ModelAPIResponses, BaseURL: "http://responses.fixture.invalid/v1", APIKey: "fixture-key", Network: policy}},
		ModelReasoningEfforts: map[string][]string{"alternate": {"high"}},
		ModelOutputLimits:     map[string]harness.ModelOutputLimits{"alternate": {MaxOutputTokens: 128}},
		Budget:                harness.BudgetLimits{MaxOutputTokens: 512, MaxModelCalls: 3}, Tools: []tool.BaseTool{underlying},
	})
	req := request("responses-proxy-roundtrip")
	req.Session.Model, req.Session.ReasoningEffort = "alternate", "high"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var text strings.Builder
	var total harness.Usage
	result, err := e.Run(ctx, req, func(_ context.Context, event harness.RunEvent) error {
		if strings.Contains(event.Text, opaque) {
			t.Error("opaque provider reasoning was exposed as a visible event")
		}
		if event.Kind == "text_delta" {
			text.WriteString(event.Text)
		}
		if event.Kind == "usage" && event.Usage != nil {
			total.InputTokens += event.Usage.InputTokens
			total.OutputTokens += event.Usage.OutputTokens
			total.TotalTokens += event.Usage.TotalTokens
			total.Estimated = total.Estimated || event.Usage.Estimated
		}
		return nil
	}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.StopReason != "end_turn" || text.String() != answer || requests.Load() != 2 || underlying.calls.Load() != 1 {
		t.Fatalf("Responses Engine.Run failed: result=%+v err=%v text=%q requests=%d tools=%d", result, err, text.String(), requests.Load(), underlying.calls.Load())
	}
	if total.InputTokens != 50 || total.OutputTokens != 9 || total.TotalTokens != 59 || total.Estimated {
		t.Fatalf("Responses usage was lost or double counted: %+v", total)
	}
}

func TestResponsesRootEngineRetriesProviderServerErrorsWithinBudget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("root API setting did not select Responses: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"type":"server_error","message":"fixture unavailable"}}`)
	}))
	defer server.Close()
	// A 503 before any stream opens is retried with backoff, but every attempt
	// is a charged model call: the run budget still bounds the retries.
	e := newTestEngine(t, Config{Provider: "openai", API: harness.ModelAPIResponses, Model: "responses-model", BaseURL: server.URL + "/v1", APIKey: "fixture-key", Budget: harness.BudgetLimits{MaxOutputTokens: 64, MaxModelCalls: 2}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := e.Run(ctx, request("responses-server-failure"), nil, nil)
	if requests.Load() != 2 || ctx.Err() != nil || err == nil && result.Limit == "" {
		t.Fatalf("server failure was not retried within budget: result=%+v err=%v requests=%d", result, err, requests.Load())
	}
}
