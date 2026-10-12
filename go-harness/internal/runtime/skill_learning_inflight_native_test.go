package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type learningInFlightBroker struct {
	skillmaintenance.Broker
	before func()
}

func (b *learningInFlightBroker) Reserve(ctx context.Context, call skillmaintenance.ModelCall) (skillmaintenance.Receipt, error) {
	if b.before != nil {
		b.before()
		b.before = nil
	}
	return b.Broker.Reserve(ctx, call)
}

type learningInFlightNativeMatcher struct {
	harness.SkillExperienceMatcher
	before func()
}

func (m *learningInFlightNativeMatcher) Match(ctx context.Context, input harness.SkillExperienceMatchInput) (harness.SkillExperienceMatchDecision, error) {
	if m.before != nil {
		broker, ok := skillmaintenance.BrokerFromContext(ctx)
		if !ok {
			return harness.SkillExperienceMatchDecision{}, harness.ErrPermissionDenied
		}
		ctx = skillmaintenance.WithBroker(ctx, &learningInFlightBroker{Broker: broker, before: m.before})
		m.before = nil
	}
	return m.SkillExperienceMatcher.Match(ctx, input)
}

func TestSkillLearningInFlightNativeMatchingPreservesAdmissionCauseAndPaidResponse(t *testing.T) {
	for _, point := range []string{"reservation", "response"} {
		t.Run(point, func(t *testing.T) {
			s, x, registry, extractor, _ := newMatchingTest(t)
			ctx := context.Background()
			started := make(chan harness.RunRequest, 1)
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				index := calls.Add(1)
				var request struct {
					Messages []struct{ Role, Content string }
					Tools    []any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 || len(request.Tools) != 0 {
					t.Error("native matcher did not use isolated maintenance input", err)
					http.Error(w, "invalid fixture request", 400)
					return
				}
				var input harness.SkillExperienceMatchInput
				if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil || !input.InventoryComplete || len(input.Skills) != 1 {
					t.Error("native matcher lost authorized input", err)
					http.Error(w, "invalid fixture input", 400)
					return
				}
				if point == "response" && index == 1 {
					started <- learningTestRun(t, s, x, "Also inspect the optional fields")
				}
				answer, _ := json.Marshal(map[string]any{"pattern_id": input.PatternID, "revision": input.Revision, "relation": "body_gap", "skill_id": input.Skills[0].ID, "reason": "Add the complete-document check", "suggested_name": ""})
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
			}))
			defer server.Close()
			native, err := einoengine.NewSkillExperienceMatcher(ctx, harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture-only"}, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			matcher := &learningInFlightNativeMatcher{SkillExperienceMatcher: native}
			if point == "reservation" {
				matcher.before = func() { started <- learningTestRun(t, s, x, "Also inspect the optional fields") }
			}
			learningInFlightRestart(t, s, registry, extractor, &consolidationTestModel{}, matcher)
			for _, text := range []string{"Inspect complete JSON configuration", "Correct the check with complete file text"} {
				learningQueueReadinessTurn(t, s, learningTestRun(t, s, x, text))
			}
			learningTestQueue(t, s)
			matchingRun(t, s)
			consolidateRun(t, s)
			matchingQueue(t, s)
			id, _ := matchingJob(t, s)
			matchingRun(t, s)
			var active harness.RunRequest
			select {
			case active = <-started:
			default:
				t.Fatal("native fixture did not start the ordinary continuation")
			}
			retry, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || active.RunID == "" || retry.State != "retry" || retry.Attempts != 1 || retry.ErrorCode != "source_active" {
				t.Fatal("native adapter erased the temporary host admission cause", retry, err)
			}
			paid := 1
			if point == "reservation" {
				paid = 0
			}
			if int(calls.Load()) != paid {
				t.Fatal("native provider called before successful reservation", calls.Load(), paid)
			}
			learningInFlightRestart(t, s, registry, extractor, &consolidationTestModel{}, matcher)
			time.Sleep(time.Until(retry.RetryAt) + 50*time.Millisecond)
			if worked, err := s.Store.Learning.RunOne(ctx); err != nil || worked {
				t.Fatal("native retry claimed a still-active conversation", worked, err)
			}
			learningQueueReadinessTurn(t, s, active)
			matchingRun(t, s)
			completed, err := skillmaintenance.ReadJob(ctx, s.Store.db, id)
			if err != nil || completed.State != "completed" || completed.Attempts != 2 || completed.Request != retry.Request || completed.Deadline != retry.Deadline || int(calls.Load()) != paid+1 {
				t.Fatal("native matcher did not resume its original input", completed, calls.Load(), err)
			}
			var count, tokens, unsettled int
			if err := s.Store.db.QueryRow(`SELECT count(*),sum(charged_tokens),sum(state<>'settled') FROM df_skill_maintenance_calls WHERE job_id=?`, id).Scan(&count, &tokens, &unsettled); err != nil || count != paid+1 || tokens != 15*count || unsettled != 0 {
				t.Fatal("native retry refunded or replayed settled accounting", count, tokens, unsettled, err)
			}
			page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
			if err != nil || len(page.Matches) != 1 || page.Matches[0].Relation != "body_gap" {
				t.Fatal("native retry did not commit its original authorized decision", page, err)
			}
		})
	}
}
