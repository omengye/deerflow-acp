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

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestCompactionUsesIndependentThinkingAndBoundedRetryOnWire(t *testing.T) {
	for _, tc := range []struct {
		name, parameter string
		cap             int
	}{
		{"enable-thinking", "enable_thinking", 4096},
		{"thinking-type", "thinking_type", 4096},
		{"reasoning-effort", "reasoning_effort", 4096},
		{"undeclared-switch", "", 4096},
		{"smaller-shared-cap", "enable_thinking", 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var summaries, answers atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				messages, _ := body["messages"].([]any)
				var contents []string
				for _, value := range messages {
					msg, _ := value.(map[string]any)
					content, _ := msg["content"].(string)
					contents = append(contents, content)
				}
				joined := strings.Join(contents, "\n")
				isSummary := strings.Contains(joined, compactionInstruction)
				wantEffort := "high"
				if isSummary {
					wantEffort = ""
					if tc.parameter == "reasoning_effort" {
						wantEffort = "none"
					}
				}
				effort, _ := body["reasoning_effort"].(string)
				if effort != wantEffort {
					t.Errorf("summary=%v effort=%q want=%q", isSummary, effort, wantEffort)
				}
				if tc.parameter == "enable_thinking" && body["enable_thinking"] != !isSummary {
					t.Errorf("summary=%v enable_thinking=%v", isSummary, body["enable_thinking"])
				}
				if tc.parameter == "thinking_type" {
					thinking, _ := body["thinking"].(map[string]any)
					want := "enabled"
					if isSummary {
						want = "disabled"
					}
					if thinking["type"] != want {
						t.Errorf("summary=%v thinking=%v", isSummary, thinking)
					}
				}
				if tc.parameter == "" && (body["enable_thinking"] != nil || body["thinking"] != nil) {
					t.Error("invented undeclared thinking switch")
				}
				if body["max_completion_tokens"] != float64(tc.cap) || body["max_tokens"] != nil {
					t.Errorf("cap changed: completion=%v legacy=%v", body["max_completion_tokens"], body["max_tokens"])
				}
				text, finish := "answer", "stop"
				if isSummary {
					attempt := summaries.Add(1)
					if strings.Contains(joined, "<analysis>") || strings.Contains(joined, "full code snippets") {
						t.Error("verbose default summary instruction survived")
					}
					if tools, _ := body["tools"].([]any); len(tools) > 0 {
						t.Error("hidden summary can invoke tools")
					}
					if attempt == 1 {
						text, finish = "discarded partial summary", "length"
					} else {
						if !strings.Contains(joined, compactionRetryInstruction) || strings.Contains(joined, "discarded partial summary") {
							t.Error("retry did not safely regenerate original history")
						}
						text = "Earlier requests answered; continue the current request."
					}
				} else {
					answers.Add(1)
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": finish}},
					"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
				})
			}))
			defer server.Close()
			e := newTestEngine(t, Config{
				Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture",
				Budget:                harness.BudgetLimits{MaxModelCalls: 3, MaxOutputTokens: tc.cap},
				ModelThinking:         map[string]harness.ThinkingConfig{"fixture": {Parameter: tc.parameter}},
				ModelReasoningEfforts: map[string][]string{"fixture": {"high", "max"}},
				Compaction:            harness.CompactionConfig{Enabled: true, ContextMessages: 4, ContextTokens: 100000, KeepRecentMessages: 1},
			})
			for i := 0; i < 3; i++ {
				req := request(fmt.Sprintf("compact-wire-%d", i))
				req.Session.Model, req.Session.ReasoningEffort = "fixture", "high"
				if tc.parameter != "" {
					req.Session.ThinkingMode = "on"
				}
				result, err := e.Run(context.Background(), req, nil, nil)
				if err != nil || result.StopReason != "end_turn" {
					t.Fatalf("turn=%d result=%+v err=%v", i, result, err)
				}
			}
			if summaries.Load() != 2 || answers.Load() != 3 {
				t.Fatalf("summaries=%d answers=%d", summaries.Load(), answers.Load())
			}
		})
	}
}
