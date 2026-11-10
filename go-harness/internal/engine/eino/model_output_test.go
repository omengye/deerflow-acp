package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestModelOutputLimitsOnWire(t *testing.T) {
	for _, tc := range []struct {
		name                                                           string
		host, requested, supported, role, request, window, total, want int
	}{
		{name: "model", host: 512, requested: 128, supported: 256, want: 128},
		{name: "inherited service ceiling", host: 512, supported: 128, want: 128},
		{name: "unknown ceiling inherits host", host: 128, want: 128},
		{name: "service ceiling without host", supported: 128, want: 128},
		{name: "role", host: 512, requested: 128, role: 64, want: 64},
		{name: "caller", host: 512, requested: 128, request: 32, want: 32},
		{name: "session hard cap", host: 64, requested: 128, want: 64},
		{name: "context", host: 512, requested: 128, window: 100, want: -1},
		{name: "remaining task budget", host: 512, requested: 128, total: 100, want: -2},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/generate", true: "/stream"}[streaming], func(t *testing.T) {
				input := []*schema.Message{schema.UserMessage("fixture")}
				estimate, err := estimateModelTokens(input, nil)
				if err != nil {
					t.Fatal(err)
				}
				want := tc.want
				if want == -1 {
					want = tc.window - int(estimate) - 16
				}
				if want == -2 {
					want = tc.total - int(estimate)
				}
				var calls atomic.Int32
				server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if body["max_completion_tokens"] != float64(want) || body["max_tokens"] != nil {
						t.Errorf("wrong wire limit: completion=%v legacy=%v want=%d", body["max_completion_tokens"], body["max_tokens"], want)
					}
					_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
				}))
				defer server.Close()
				ctx := context.Background()
				chat, err := newModel(ctx, Config{Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: server.URL}, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				budget := &runBudget{limits: harness.BudgetLimits{MaxOutputTokens: tc.host, MaxTokens: int64(tc.total)}}
				tracked := &trackedModel{inner: chat, io: newRunIO(ctx), budget: budget, sink: &eventSink{}, contextWindow: tc.window, outputLimits: harness.ModelOutputLimits{MaxOutputTokens: tc.requested, MaxSupportedOutputTokens: tc.supported}}
				if tc.role > 0 {
					tracked.settings.MaxTokens = &tc.role
				}
				var opts []model.Option
				if tc.request > 0 {
					opts = append(opts, model.WithMaxTokens(tc.request))
				}
				if streaming {
					reader, err := tracked.Stream(ctx, input, opts...)
					if err != nil {
						t.Fatal(err)
					}
					for {
						_, err := reader.Recv()
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					reader.Close()
				} else if _, err := tracked.Generate(ctx, input, opts...); err != nil {
					t.Fatal(err)
				}
				tracked.io.closeAndWait()
				if calls.Load() != 1 || budget.held != 0 || budget.spent != 11 {
					t.Fatalf("calls=%d held=%d spent=%d", calls.Load(), budget.held, budget.spent)
				}
			})
		}
	}
}

func TestModelOutputLimitsSelectedRouteAndDurableReservation(t *testing.T) {
	for _, selected := range []string{"root", "alternate"} {
		t.Run(selected, func(t *testing.T) {
			limits := harness.BudgetLimits{MaxOutputTokens: 512}
			store, ledger, req, scope := durableFixture(t, "model-limits", limits)
			want := 64
			if selected == "alternate" {
				want = 128
			}
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				call := calls.Add(1)
				cap := want
				if call == 2 {
					cap = 32
				}
				if body["model"] != selected || body["max_completion_tokens"] != float64(cap) {
					t.Errorf("selected route did not enforce limit: model=%v cap=%v", body["model"], body["max_completion_tokens"])
				}
				_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
			}))
			defer server.Close()
			cfg := Config{Provider: "openai", Model: "root", BaseURL: server.URL, APIKey: "fixture", Budget: limits, BudgetLedger: ledger,
				ModelRoutes:       map[string]harness.ModelRoute{"alternate": {Provider: "openai", Model: "alternate", BaseURL: server.URL, APIKey: "fixture"}},
				ModelOutputLimits: map[string]harness.ModelOutputLimits{"root": {MaxOutputTokens: 64}, "alternate": {MaxOutputTokens: 128, MaxSupportedOutputTokens: 256}}}
			cfg.ExtensionFactory = func(context.Context, harness.RunRequest, json.RawMessage) (RunExtensions, error) {
				return RunExtensions{PostRunFactory: func(_ context.Context, chat model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
					return func(ctx context.Context, _ string, _ bool) error {
						_, err := chat.Generate(ctx, []*schema.Message{schema.UserMessage("summary")}, model.WithMaxTokens(32))
						return err
					}, nil
				}}, nil
			}
			e := newTestEngine(t, cfg)
			req.Session.Model = selected
			result, err := e.Run(durablebudget.WithScope(context.Background(), scope), req, nil, nil)
			if err != nil || result.StopReason != "end_turn" || calls.Load() != 2 {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
			}
			rows, err := store.DB().Query(`SELECT output_cap FROM budget_reservations WHERE root_id=? AND kind='model' ORDER BY output_cap`, scope.RootBudgetID)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var caps []int
			for rows.Next() {
				var cap int
				if err := rows.Scan(&cap); err != nil {
					t.Fatal(err)
				}
				caps = append(caps, cap)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(caps) != 2 || caps[0] != 32 || caps[1] != want {
				t.Fatalf("durable reservation disagrees with wire: %v", caps)
			}
		})
	}
}

func TestModelOutputLimitsBindExecutionContract(t *testing.T) {
	e := &Engine{config: Config{Model: "fixture"}}
	before, err := e.executionContract(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.config.ModelOutputLimits = map[string]harness.ModelOutputLimits{"fixture": {MaxOutputTokens: 128}}
	after, err := e.executionContract(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("model output policy did not change execution contract")
	}
}

// Reproduce the hy4free failure through the real OpenAI-compatible adapter,
// including the final usage-only SSE chunk that arrives after finish_reason.
func TestReasoningOnlyWireStopsAtFixedAllowance(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["max_completion_tokens"] != float64(4096) || body["reasoning_effort"] != "high" {
			t.Errorf("fixed settings changed: cap=%v effort=%v", body["max_completion_tokens"], body["reasoning_effort"])
		}
		_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"PRIVATE_FIXTURE_REASONING"},"finish_reason":"length"}],"usage":{"prompt_tokens":25813,"completion_tokens":4096,"total_tokens":29909,"completion_tokens_details":{"reasoning_tokens":4096}}}`)
	}))
	defer server.Close()
	e := newTestEngine(t, Config{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture", Budget: harness.BudgetLimits{MaxOutputTokens: 4096}, ModelReasoningEfforts: map[string][]string{"fixture": {"high"}}, ModelOutputLimits: map[string]harness.ModelOutputLimits{"fixture": {MaxOutputTokens: 4096, MaxSupportedOutputTokens: 64000}}})
	req := request("reasoning-wire")
	req.Session.ReasoningEffort = "high"
	var notice string
	result, err := e.Run(context.Background(), req, func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "output_limit" {
			notice += event.Text
		}
		return nil
	}, nil)
	if err != nil || result.Limit != "output_tokens" || result.StopReason != "max_tokens" || calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	if !strings.Contains(notice, "4096 tokens (4096 reasoning tokens)") {
		t.Fatalf("usage tail lost: %s", notice)
	}
}
