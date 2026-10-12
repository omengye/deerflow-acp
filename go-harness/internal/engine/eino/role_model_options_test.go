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

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestRoleProviderOptionsReachWireWithinBudget(t *testing.T) {
	for _, tc := range []struct {
		name, provider, effort, thinkingType string
		thinking                             *bool
		thinkingTokens                       int
	}{
		{"openai-reasoning", "openai", "high", "", nil, 0},
		{"openai-xhigh", "openai", "xhigh", "", nil, 0},
		{"claude-thinking", "claude", "", "enabled", rolePointer(true), 1200},
		{"claude-disabled", "claude", "", "disabled", rolePointer(false), 0},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "fixture", 400)
						return
					}
					if body["reasoning_effort"] != nil && body["reasoning_effort"] != tc.effort || tc.effort != "" && body["reasoning_effort"] != tc.effort {
						t.Errorf("reasoning_effort=%v", body["reasoning_effort"])
					}
					if tc.thinkingType != "" {
						thinking, _ := body["thinking"].(map[string]any)
						if thinking["type"] != tc.thinkingType {
							t.Errorf("thinking=%v", thinking)
						}
						if tc.thinkingTokens > 0 && thinking["budget_tokens"] != float64(tc.thinkingTokens) {
							t.Errorf("thinking budget=%v", thinking)
						}
					}
					cap, absent := "max_completion_tokens", "max_tokens"
					if tc.provider == "claude" {
						cap, absent = absent, cap
					}
					if body[cap] != float64(1536) || body[absent] != nil {
						t.Errorf("token limits: %s=%v %s=%v", cap, body[cap], absent, body[absent])
					}
					if body["model"] != "fixture-model" {
						t.Error("lost selected model")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.provider == "claude" {
						fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"fixture-model\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
					}
				}))
				defer server.Close()
				ctx := context.Background()
				cfg := Config{Provider: tc.provider, Model: "fixture-model", APIKey: "fixture", BaseURL: server.URL, Budget: harness.BudgetLimits{MaxTokens: 20000, MaxOutputTokens: 1536}}
				chat, err := newModel(ctx, cfg, cfg.Model)
				if err != nil {
					t.Fatal(err)
				}
				spec := agents.Spec{Model: cfg.Model, ThinkingEnabled: tc.thinking, ReasoningEffort: tc.effort, ModelSettings: harness.AgentModelSettings{MaxTokens: rolePointer(4000)}}
				if tc.thinkingTokens != 0 {
					spec.ModelSettings.ThinkingBudgetTokens = rolePointer(tc.thinkingTokens)
				}
				tuning, err := (&Engine{config: cfg}).roleTuning(spec)
				if err != nil {
					t.Fatal(err)
				}
				budget := &runBudget{limits: cfg.Budget}
				lifecycle := newRunIO(ctx)
				tracked := &trackedModel{inner: chat, io: lifecycle, budget: budget, sink: &eventSink{}, settings: spec.ModelSettings, tuning: tuning}
				input := []*schema.Message{schema.UserMessage("fixture")}
				var answer string
				if stream {
					reader, err := tracked.Stream(ctx, input, model.WithTemperature(1))
					if err != nil {
						t.Fatal(err)
					}
					defer reader.Close()
					for {
						chunk, err := reader.Recv()
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						answer += chunk.Content
					}
				} else {
					result, err := tracked.Generate(ctx, input, model.WithTemperature(1))
					if err != nil {
						t.Fatal(err)
					}
					answer = result.Content
				}
				lifecycle.closeAndWait()
				if answer != "done" || calls.Load() != 1 || budget.held != 0 || budget.spent != 11 {
					t.Fatalf("answer=%q calls=%d budget=%+v", answer, calls.Load(), budget)
				}
			})
		}
	}
}

func TestRoleThinkingStopsBeforeProviderWhenRemainingCapShrinks(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ctx := context.Background()
			fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				t.Error("provider invoked with invalid thinking budget")
				return textStream("unexpected"), nil
			}}
			budget := &runBudget{limits: harness.BudgetLimits{MaxTokens: 500, MaxOutputTokens: 4096}}
			tracked := &trackedModel{inner: fake, io: newRunIO(ctx), budget: budget, sink: &eventSink{}, tuning: &roleModelTuning{provider: "claude", thinking: rolePointer(true), thinkingTokens: 1024}}
			var err error
			if stream {
				_, err = tracked.Stream(ctx, []*schema.Message{schema.UserMessage("hello")})
			} else {
				_, err = tracked.Generate(ctx, []*schema.Message{schema.UserMessage("hello")})
			}
			tracked.io.closeAndWait()
			var quota *budgetError
			if !errors.As(err, &quota) || quota.resource != "tokens" || fake.calls != 0 || budget.spent != 0 || budget.held != 0 {
				t.Fatalf("err=%v budget=%+v calls=%d", err, budget, fake.calls)
			}
		})
	}
}

func TestConfiguredRoleProviderValidationAtStartup(t *testing.T) {
	for _, tc := range []struct {
		provider string
		role     harness.AgentConfig
		want     string
	}{
		{"openai", harness.AgentConfig{ThinkingEnabled: rolePointer(false)}, "thinking_enabled"},
		{"claude", harness.AgentConfig{ReasoningEffort: "high"}, "reasoning_effort"},
		{"claude", harness.AgentConfig{ThinkingEnabled: rolePointer(true), ModelSettings: harness.AgentModelSettings{Temperature: rolePointer(0.5)}}, "temperature"},
		{"claude", harness.AgentConfig{ThinkingEnabled: rolePointer(true), ModelSettings: harness.AgentModelSettings{MaxTokens: rolePointer(1024)}}, "thinking_budget_tokens"},
	} {
		t.Run(tc.provider+"/"+tc.want, func(t *testing.T) {
			tc.role.Model = "child"
			_, err := New(context.Background(), Config{Model: "root", ChatModel: &scriptedModel{}, ModelRoutes: map[string]harness.ModelRoute{"child": {Provider: tc.provider, Model: "unused"}}, Subagents: &harness.SubagentsConfig{Agents: map[string]harness.AgentConfig{"general-purpose": tc.role}}})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "subagents.agents.general-purpose") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
