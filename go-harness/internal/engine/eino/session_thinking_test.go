package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSessionThinkingModelWire(t *testing.T) {
	for _, parameter := range []string{"enable_thinking", "thinking_type", "reasoning_effort", "claude"} {
		t.Run(parameter, func(t *testing.T) {
			requests := make(chan map[string]any, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				if parameter == "claude" {
					fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"fixture\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			provider, efforts := "openai-compatible", []string{"low", "high"}
			if parameter == "claude" {
				provider, efforts = "claude", nil
			}
			e := newTestEngine(t, Config{Provider: provider, Model: "fixture", APIKey: "fixture", BaseURL: server.URL,
				ModelReasoningEfforts: map[string][]string{"fixture": efforts},
				ModelThinking:         map[string]harness.ThinkingConfig{"fixture": {Parameter: parameter}},
				Budget:                harness.BudgetLimits{MaxOutputTokens: 2048},
			})
			for i, mode := range []string{"on", "off", "on", "default"} {
				req := request(fmt.Sprintf("thinking-%d", i))
				req.Session.ID = fmt.Sprintf("session-%d", i)
				req.Session.Model, req.Session.ThinkingMode = "fixture", mode
				if parameter != "claude" {
					req.Session.ReasoningEffort = "high"
				}
				result, err := e.Run(context.Background(), req, nil, nil)
				if err != nil || result.StopReason != "end_turn" {
					t.Fatalf("%s: result=%+v err=%v", mode, result, err)
				}
				body := <-requests
				var wantEffort any
				if parameter != "claude" {
					wantEffort = "high"
				}
				if parameter == "reasoning_effort" && mode == "off" {
					wantEffort = "none"
				}
				if body["reasoning_effort"] != wantEffort {
					t.Fatalf("%s effort=%v want %v", mode, body["reasoning_effort"], wantEffort)
				}
				var wantThinking any
				if mode != "default" {
					switch parameter {
					case "enable_thinking":
						wantThinking = mode == "on"
					case "thinking_type", "claude":
						kind := "disabled"
						if mode == "on" {
							kind = "enabled"
						}
						value := map[string]any{"type": kind}
						if parameter == "claude" && mode == "on" {
							value["budget_tokens"] = float64(1024)
						}
						wantThinking = value
					}
				}
				key, cap := "thinking", "max_completion_tokens"
				if parameter == "enable_thinking" {
					key = "enable_thinking"
				}
				if parameter == "claude" {
					cap = "max_tokens"
				}
				if !reflect.DeepEqual(body[key], wantThinking) {
					t.Fatalf("%s %s=%v want %v", mode, key, body[key], wantThinking)
				}
				if body[cap] != float64(2048) {
					t.Fatalf("output budget bypass: %v", body[cap])
				}
			}
		})
	}
}

func TestSessionThinkingDefaultEffortAndIdentity(t *testing.T) {
	e := &Engine{config: Config{Provider: "openai", Model: "m", ModelThinking: map[string]harness.ThinkingConfig{"m": {Parameter: "reasoning_effort"}}, ModelReasoningEfforts: map[string][]string{"m": {"low", "high"}}}}
	if _, err := e.sessionTuning("m", harness.Session{ThinkingMode: "on"}); err == nil {
		t.Fatal("explicit on silently selected the first effort")
	}
	tuning, err := e.sessionTuning("m", harness.Session{ThinkingMode: "on", ReasoningEffort: "high"})
	if err != nil || tuning.effort != "high" {
		t.Fatalf("explicit on tuning=%+v err=%v", tuning, err)
	}
	for _, mode := range []string{"", "default"} {
		tuning, err := e.sessionTuning("m", harness.Session{ThinkingMode: mode})
		if err != nil || tuning != nil && tuning.effort != "" {
			t.Fatalf("provider default was overridden: %+v %v", tuning, err)
		}
	}
	if _, err := e.sessionTuning("unknown", harness.Session{ThinkingMode: "off"}); err == nil {
		t.Fatal("unsupported switch accepted")
	}
	req := request("identity")
	before := itemFor(req)
	req.Session.ThinkingMode = "off"
	if before == itemFor(req) {
		t.Fatal("thinking omitted from resume identity")
	}
	contract, err := e.executionContract(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.config.ModelThinking["m"] = harness.ThinkingConfig{Parameter: "enable_thinking"}
	changed, err := e.executionContract(nil, nil)
	if err != nil || contract == changed {
		t.Fatal("wire contract change did not invalidate checkpoint")
	}
}
