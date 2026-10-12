package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestOutputTokenParameterWire(t *testing.T) {
	for _, provider := range []string{"openai", "openai-compatible"} {
		for _, parameter := range []harness.OutputTokenParameter{harness.OutputTokensAuto, harness.OutputTokensLegacy, harness.OutputTokensCompletion} {
			for _, effort := range []string{"", "high"} {
				for _, streaming := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/effort=%s/stream=%t", provider, parameter, effort, streaming), func(t *testing.T) {
						var calls atomic.Int32
						server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							var body map[string]any
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
								t.Error(err)
								return
							}
							field, _ := harness.ResolveOutputTokenParameter(provider, "", parameter)
							other := "max_tokens"
							if field == harness.OutputTokensLegacy {
								other = "max_completion_tokens"
							}
							if body[string(field)] != float64(64) || body[other] != nil {
								t.Errorf("wrong cap: %s=%v %s=%v", field, body[string(field)], other, body[other])
							}
							if body["temperature"] != 0.25 || len(body["tools"].([]any)) != 1 {
								t.Errorf("lost unrelated options: %v", body)
							}
							if effort == "" && body["reasoning_effort"] != nil || effort != "" && body["reasoning_effort"] != effort {
								t.Errorf("effort=%v", body["reasoning_effort"])
							}
							_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
						}))
						defer server.Close()
						ctx := context.Background()
						cfg := Config{Provider: provider, Model: "fixture", APIKey: "fixture", BaseURL: server.URL, OutputTokenParameter: parameter}
						chat, err := newModel(ctx, cfg, cfg.Model)
						if err != nil {
							t.Fatal(err)
						}
						chat, err = chat.WithTools([]*schema.ToolInfo{{Name: "noop", Desc: "Do nothing"}})
						if err != nil {
							t.Fatal(err)
						}
						tuning, err := (&Engine{config: cfg}).roleTuning(agents.Spec{Model: cfg.Model, ReasoningEffort: effort})
						if err != nil {
							t.Fatal(err)
						}
						budget := &runBudget{limits: harness.BudgetLimits{MaxOutputTokens: 64}}
						tracked := &trackedModel{inner: chat, io: newRunIO(ctx), budget: budget, sink: &eventSink{}, tuning: tuning, settings: harness.AgentModelSettings{MaxTokens: rolePointer(512)}}
						input := []*schema.Message{schema.UserMessage("fixture")}
						opts := []model.Option{model.WithMaxTokens(1000), model.WithTemperature(0.25)}
						var answer string
						if streaming {
							reader, err := tracked.Stream(ctx, input, opts...)
							if err != nil {
								t.Fatal(err)
							}
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
							reader.Close()
						} else {
							msg, err := tracked.Generate(ctx, input, opts...)
							if err != nil {
								t.Fatal(err)
							}
							answer = msg.Content
						}
						tracked.io.closeAndWait()
						if answer != "done" || calls.Load() != 1 || budget.held != 0 || budget.spent != 11 {
							t.Fatalf("answer=%q calls=%d held=%d spent=%d", answer, calls.Load(), budget.held, budget.spent)
						}
					})
				}
			}
		}
	}
}

func TestOutputTokenPolicySelectedRouteAndPostRun(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		call := calls.Add(1)
		cap := float64(96)
		if call == 2 {
			cap = 32
		}
		if body["model"] != "alternate-id" || body["max_tokens"] != cap || body["max_completion_tokens"] != nil {
			t.Errorf("route/cap not applied: %v", body)
		}
		_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	cfg := Config{Provider: "openai", Model: "default", BaseURL: server.URL, APIKey: "fixture", OutputTokenParameter: harness.OutputTokensCompletion, Budget: harness.BudgetLimits{MaxOutputTokens: 96}, ModelRoutes: map[string]harness.ModelRoute{"alternate": {Provider: "openai", Model: "alternate-id", BaseURL: server.URL, APIKey: "fixture", OutputTokenParameter: harness.OutputTokensLegacy}}}
	cfg.ExtensionFactory = func(context.Context, harness.RunRequest, json.RawMessage) (RunExtensions, error) {
		return RunExtensions{PostRunFactory: func(_ context.Context, chat model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
			return func(ctx context.Context, _ string, _ bool) error {
				_, err := chat.Generate(ctx, []*schema.Message{schema.UserMessage("memory fixture")}, model.WithMaxTokens(32))
				return err
			}, nil
		}}, nil
	}
	e := newTestEngine(t, cfg)
	req := request("route-output")
	req.Session.Model = "alternate"
	result, err := e.Run(context.Background(), req, nil, nil)
	if err != nil || result.StopReason != "end_turn" || calls.Load() != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}

func TestOutputTokenPolicyBindsExecutionContract(t *testing.T) {
	e := &Engine{config: Config{Model: "fixture"}}
	a, err := e.executionContract(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.config.OutputTokenParameter = harness.OutputTokensLegacy
	b, err := e.executionContract(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("output policy did not change execution contract")
	}
}

func TestOutputTokenPolicyRoleRouting(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		if body["model"] != "child-id" || body["max_tokens"] != float64(24) || body["max_completion_tokens"] != nil {
			t.Errorf("role route/cap not applied: %v", body)
		}
		modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	e := &Engine{config: Config{Provider: "openai", Model: "root", ModelRoutes: map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child-id", BaseURL: server.URL, APIKey: "fixture", OutputTokenParameter: harness.OutputTokensLegacy}}}}
	chat, err := e.roleModel(context.Background(), "child", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := e.roleModel(context.Background(), "child", "child", chat)
	if err != nil || inherited != chat {
		t.Fatalf("inheritance replaced selected policy: %v", err)
	}
	if _, err := inherited.Generate(context.Background(), []*schema.Message{schema.UserMessage("fixture")}, model.WithMaxTokens(24)); err != nil || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestOutputTokenPolicyMaintenance(t *testing.T) {
	_, _, _, ctx := maintenanceReviewFixture(t, 3)
	var calls atomic.Int32
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		if body["max_tokens"] != float64(512) || body["max_completion_tokens"] != nil {
			t.Errorf("maintenance route/cap not applied: %v", body)
		}
		answer := `{"base_version":"base-digest","candidate_version":"candidate-digest","passed":true,"reason":"fixture"}`
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
	}))
	defer server.Close()
	route := harness.ModelRoute{Provider: "openai", Model: "reviewer", BaseURL: server.URL, APIKey: "fixture", OutputTokenParameter: harness.OutputTokensLegacy}
	validator, err := NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route, Evaluation: route, MaxOutputTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	report, err := validator.Validate(ctx, reviewInput())
	if err != nil || calls.Load() != 2 || report.Security != "passed" {
		t.Fatalf("calls=%d report=%+v err=%v", calls.Load(), report, err)
	}
}

func TestOutputTokenPolicyHonorsNarrowerCaps(t *testing.T) {
	for _, tc := range []struct {
		name               string
		role, call, window int
		want               int
	}{
		{"host", 512, 1000, 0, 64},
		{"role", 32, 1000, 0, 32},
		{"caller", 512, 16, 0, 16},
		{"window", 512, 1000, 70, 0},
	} {
		for _, parameter := range []harness.OutputTokenParameter{harness.OutputTokensLegacy, harness.OutputTokensCompletion} {
			t.Run(tc.name+"/"+string(parameter), func(t *testing.T) {
				var got float64
				server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					got, _ = body[string(parameter)].(float64)
					modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()
				chat, err := newModel(context.Background(), Config{Provider: "openai", BaseURL: server.URL, APIKey: "fixture", OutputTokenParameter: parameter}, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				tracked := &trackedModel{inner: chat, io: newRunIO(context.Background()), budget: &runBudget{limits: harness.BudgetLimits{MaxOutputTokens: 64}}, sink: &eventSink{}, contextWindow: tc.window, settings: harness.AgentModelSettings{MaxTokens: rolePointer(tc.role)}}
				_, err = tracked.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")}, model.WithMaxTokens(tc.call))
				tracked.io.closeAndWait()
				if err != nil || got <= 0 || got > 64 || tc.want > 0 && got != float64(tc.want) || tc.window > 0 && got >= 64 {
					t.Fatalf("got=%v want=%d err=%v", got, tc.want, err)
				}
			})
		}
	}
}

func TestProviderOutputStopReasons(t *testing.T) {
	for reason, want := range map[string]string{"stop": "end_turn", "end_turn": "end_turn", "length": "max_tokens", "max_tokens": "max_tokens", "content_filter": "refusal", "refusal": "refusal"} {
		t.Run(reason, func(t *testing.T) {
			result := harness.RunResult{StopReason: "end_turn"}
			variant := &adk.MessageVariant{Role: schema.Assistant, Message: &schema.Message{Role: schema.Assistant, Content: "partial", ResponseMeta: &schema.ResponseMeta{FinishReason: reason}}}
			observed, err := consumeMessage(context.Background(), variant, &eventSink{}, &result)
			if err != nil || observed.text != "partial" || result.StopReason != want {
				t.Fatalf("observed=%+v result=%+v err=%v", observed, result, err)
			}
		})
	}
}
