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

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestConfiguredRoleCompactionUsesIndependentThinkingWithinRoleCap(t *testing.T) {
	const roleCap = 256
	var childCalls, summaries atomic.Int32
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad fixture request", http.StatusBadRequest)
			return
		}
		if body["model"] != "role-wire-model" || r.Header.Get("Authorization") != "Bearer child-fixture-key" {
			t.Error("role summary used the wrong model route or credentials")
		}
		if body["max_completion_tokens"] != float64(roleCap) || body["max_tokens"] != nil {
			t.Errorf("role output cap changed: completion=%v legacy=%v", body["max_completion_tokens"], body["max_tokens"])
		}
		messages, _ := body["messages"].([]any)
		var contents []string
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			text, _ := message["content"].(string)
			contents = append(contents, text)
		}
		joined := strings.Join(contents, "\n")
		isSummary := strings.Contains(joined, compactionInstruction)
		message := map[string]any{"role": "assistant", "content": "child complete"}
		finish := "stop"
		if isSummary {
			attempt := summaries.Add(1)
			if body["enable_thinking"] != false || body["reasoning_effort"] != nil || body["temperature"] != nil {
				t.Errorf("summary inherited role tuning: thinking=%v effort=%v temperature=%v", body["enable_thinking"], body["reasoning_effort"], body["temperature"])
			}
			if tools, _ := body["tools"].([]any); len(tools) != 0 {
				t.Error("role summary received callable tools")
			}
			if attempt == 1 {
				message["content"], finish = "REJECTED_ROLE_SUMMARY", "length"
			} else {
				if strings.Contains(joined, "REJECTED_ROLE_SUMMARY") {
					t.Error("truncated role summary contaminated retry input")
				}
				if attempt == 2 && !strings.Contains(joined, compactionRetryInstruction) {
					t.Error("role summary did not use bounded shorter-summary retry")
				}
				message["content"] = "Read-only workspace inspections completed. Continue the requested remaining inspections."
			}
		} else {
			call := childCalls.Add(1)
			if body["reasoning_effort"] != "high" || body["temperature"] != 0.25 || body["enable_thinking"] != nil {
				t.Errorf("normal role recipe changed: effort=%v temperature=%v thinking=%v", body["reasoning_effort"], body["temperature"], body["enable_thinking"])
			}
			if !strings.Contains(joined, "ROLE_SYSTEM") || !strings.Contains(joined, "CHILD_PUBLIC_TASK") {
				t.Error("role compaction lost its instructions or active request")
			}
			if call <= 4 {
				message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
					"id": fmt.Sprintf("child-read-%d", call), "type": "function",
					"function": map[string]string{"name": "read_workspace", "arguments": "{}"},
				}}}
				finish = "tool_calls"
			}
		}
		if err := modelstreamtest.WriteCompletion(w, map[string]any{
			"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}); err != nil {
			t.Error(err)
		}
	}))
	defer provider.Close()
	parent := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "delegate-role", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"researcher","prompt":"CHILD_PUBLIC_TASK","description":"read evidence"}`},
			}}}}), nil
		}
		for _, message := range input {
			if message.Role == schema.Tool && strings.Contains(message.Content, "child complete") {
				return textStream("parent complete"), nil
			}
		}
		t.Error("child result did not reach the parent after compaction")
		return textStream("unexpected parent result"), nil
	}}
	underlying := &recordingTool{}
	engine, err := New(context.Background(), Config{
		Model: "parent", Instruction: "HOST_SYSTEM", ChatModel: parent, Tools: []tool.BaseTool{underlying},
		Budget:                harness.BudgetLimits{MaxModelCalls: 20, MaxOutputTokens: 4096},
		ModelRoutes:           map[string]harness.ModelRoute{"research-route": {Provider: "openai", Model: "role-wire-model", BaseURL: provider.URL + "/v1", APIKey: "child-fixture-key"}},
		ModelThinking:         map[string]harness.ThinkingConfig{"research-route": {Parameter: "enable_thinking"}},
		ModelReasoningEfforts: map[string][]string{"research-route": {"high", "max"}},
		Compaction:            harness.CompactionConfig{Enabled: true, ContextMessages: 6, ContextTokens: 100000, KeepRecentMessages: 2},
		Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{
			"researcher": {Description: "Read evidence", SystemPrompt: "ROLE_SYSTEM", Model: "research-route", Tools: []string{"read_workspace"}, ReasoningEffort: "high", ModelSettings: harness.AgentModelSettings{Temperature: rolePointer(0.25), MaxTokens: rolePointer(roleCap)}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(context.Background(), request("configured-role-compaction"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || childCalls.Load() != 5 || summaries.Load() < 2 || underlying.calls.Load() != 4 {
		t.Fatalf("result=%+v childCalls=%d summaries=%d tools=%d err=%v", result, childCalls.Load(), summaries.Load(), underlying.calls.Load(), err)
	}
}

func TestCompactionClaudeDisablesThinkingWithoutChangingRoleRecipe(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad fixture request", http.StatusBadRequest)
			return
		}
		if body["model"] != "claude-fixture" || body["max_tokens"] != float64(1536) || body["max_completion_tokens"] != nil {
			t.Errorf("wrong Claude model or cap: model=%v cap=%v", body["model"], body["max_tokens"])
		}
		thinking, _ := body["thinking"].(map[string]any)
		want := "enabled"
		if call == 1 {
			want = "disabled"
		}
		if thinking["type"] != want || call == 1 && thinking["budget_tokens"] != nil || call == 2 && thinking["budget_tokens"] != float64(1024) {
			t.Errorf("call=%d Claude thinking=%v want=%s", call, thinking, want)
		}
		if body["reasoning_effort"] != nil || body["temperature"] != nil || body["enable_thinking"] != nil {
			t.Error("Claude request received incompatible tuning")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-fixture\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	cfg := Config{Provider: "claude", Model: "claude-fixture", BaseURL: provider.URL, APIKey: "fixture", ModelThinking: map[string]harness.ThinkingConfig{"claude-fixture": {Parameter: "claude"}}}
	e := &Engine{config: cfg}
	chat, err := newModel(ctx, cfg, cfg.Model)
	if err != nil {
		t.Fatal(err)
	}
	spec := agents.Spec{Model: cfg.Model, ThinkingEnabled: rolePointer(true), ModelSettings: harness.AgentModelSettings{MaxTokens: rolePointer(1536), ThinkingBudgetTokens: rolePointer(1024)}}
	mainTuning, err := e.roleTuning(spec)
	if err != nil {
		t.Fatal(err)
	}
	budget := &runBudget{limits: harness.BudgetLimits{MaxModelCalls: 2, MaxOutputTokens: 4096}}
	lifecycle := newRunIO(ctx)
	summaryTracked := &trackedModel{inner: chat, io: lifecycle, budget: budget, sink: &eventSink{}, settings: harness.AgentModelSettings{MaxTokens: spec.ModelSettings.MaxTokens}, tuning: e.compactionTuning(cfg.Model)}
	_, err = (&compactionModel{inner: summaryTracked}).Generate(ctx, []*schema.Message{schema.UserMessage(compactionInstruction)}, model.WithMaxTokens(4096))
	if err != nil {
		t.Fatal(err)
	}
	mainTracked := &trackedModel{inner: chat, io: lifecycle, budget: budget, sink: &eventSink{}, settings: spec.ModelSettings, tuning: mainTuning}
	if _, err = mainTracked.Generate(ctx, []*schema.Message{schema.UserMessage("Continue the role task.")}); err != nil {
		t.Fatal(err)
	}
	lifecycle.closeAndWait()
	if calls.Load() != 2 || budget.held != 0 || budget.spent != 22 {
		t.Fatalf("calls=%d held=%d spent=%d", calls.Load(), budget.held, budget.spent)
	}
}
