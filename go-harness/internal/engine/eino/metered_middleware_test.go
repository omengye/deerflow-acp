package eino

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

type meteredMiddlewareProbe struct {
	*adk.BaseChatModelAgentMiddleware
	model  model.BaseModel[*schema.Message]
	called bool
}

func (p *meteredMiddlewareProbe) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if !p.called {
		p.called = true
		_, err := p.model.Generate(ctx, []*schema.Message{schema.UserMessage("extract memory candidate")})
		if err != nil {
			return ctx, state, err
		}
	}
	return ctx, state, nil
}

func TestMiddlewareModelCallsUseOriginalDurableBudget(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			limits := harness.BudgetLimits{MaxModelCalls: limit, MaxTokens: 100000, MaxOutputTokens: 64}
			_, ledger, req, scope := durableFixture(t, "middleware-budget", limits)
			fake := &scriptedModel{stream: func(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if len(input) == 1 && input[0].Content == "extract memory candidate" {
					return textStream("candidate"), nil
				}
				return textStream("main answer"), nil
			}}
			e := newTestEngine(t, Config{ChatModel: fake, Budget: limits, BudgetLedger: ledger,
				ExtensionFactory: func(context.Context, harness.RunRequest, json.RawMessage) (RunExtensions, error) {
					return RunExtensions{ModelHandlerFactory: func(_ context.Context, metered model.BaseModel[*schema.Message]) ([]adk.ChatModelAgentMiddleware, error) {
						return []adk.ChatModelAgentMiddleware{&meteredMiddlewareProbe{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, model: metered}}, nil
					}}, nil
				},
			})
			result, err := e.Run(durablebudget.WithScope(context.Background(), scope), req, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantReason := "end_turn"
			if limit == 1 {
				wantReason = "max_turn_requests"
			}
			if result.StopReason != wantReason || fake.calls != limit {
				t.Fatalf("result=%+v provider calls=%d limit=%d", result, fake.calls, limit)
			}
			snapshot, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
			if err != nil || snapshot.ModelCalls != limit || snapshot.HeldTokens != 0 {
				t.Fatalf("shared budget=%+v err=%v", snapshot, err)
			}
		})
	}
}
