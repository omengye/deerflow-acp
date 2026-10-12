package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func TestSkillLearningMatchingReassessmentNativeInventoryChangeIsNotModelRetry(t *testing.T) {
	for _, point := range []string{"reservation", "response"} {
		t.Run(point, func(t *testing.T) {
			s, x, registry, extractor, _ := newMatchingTest(t)
			ctx := context.Background()
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				index := calls.Add(1)
				var request struct {
					Messages []struct{ Role, Content string }
					Tools    []any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 || len(request.Tools) != 0 {
					t.Error("native matching did not use isolated maintenance input", err)
					http.Error(w, "invalid fixture request", 400)
					return
				}
				var input harness.SkillExperienceMatchInput
				if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil || !input.InventoryComplete || len(input.Skills) != 1 {
					t.Error("native matching lost authorized input", err)
					http.Error(w, "invalid fixture input", 400)
					return
				}
				if point == "response" && index == 1 {
					matchingReassessmentChange(t, s, registry)
				}
				answer, _ := json.Marshal(map[string]any{"pattern_id": input.PatternID, "revision": input.Revision, "relation": "already_covered", "skill_id": input.Skills[0].ID, "reason": "The current body and trigger cover this procedure", "suggested_name": ""})
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
			}))
			defer server.Close()
			native, err := einoengine.NewSkillExperienceMatcher(ctx, harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture-only"}, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			matcher := &learningInFlightNativeMatcher{SkillExperienceMatcher: native}
			if point == "reservation" {
				matcher.before = func() { matchingReassessmentChange(t, s, registry) }
			}
			learningInFlightRestart(t, s, registry, extractor, &consolidationTestModel{}, matcher)
			for _, text := range []string{"Inspect complete JSON input", "Validate required fields from full text"} {
				learningQueueReadinessTurn(t, s, learningTestRun(t, s, x, text))
			}
			learningTestQueue(t, s)
			matchingRun(t, s)
			consolidateRun(t, s)
			matchingQueue(t, s)
			oldID, _ := matchingJob(t, s)
			matchingRun(t, s)
			old, err := skillmaintenance.ReadJob(ctx, s.Store.db, oldID)
			if err != nil || old.State != "failed" || old.ErrorCode != "inventory_changed" || old.Attempts != 1 {
				t.Fatal("native adapter erased the host inventory cause or retried old work", old, err)
			}
			paid := boolInt(point == "response")
			if int(calls.Load()) != paid {
				t.Fatal("native provider called before inventory admission", calls.Load(), paid)
			}
			learningInFlightRestart(t, s, registry, extractor, &consolidationTestModel{}, matcher)
			matchingQueue(t, s)
			freshID, _ := matchingJob(t, s)
			matchingRun(t, s)
			fresh, err := skillmaintenance.ReadJob(ctx, s.Store.db, freshID)
			if err != nil || freshID == oldID || fresh.State != "completed" || fresh.Attempts != 1 || fresh.Request.Reference == old.Request.Reference || fresh.Request.InputDigest == old.Request.InputDigest || int(calls.Load()) != paid+1 {
				t.Fatal("native automatic reassessment was not a fresh paid snapshot", fresh, calls.Load(), err)
			}
			var count, tokens, unsettled int
			if err := s.Store.db.QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0),COALESCE(sum(state<>'settled'),0) FROM df_skill_maintenance_calls WHERE job_id IN (?,?)`, oldID, freshID).Scan(&count, &tokens, &unsettled); err != nil || count != paid+1 || tokens != 15*count || unsettled != 0 {
				t.Fatal("native reassessment refunded or reclassified accounting", count, tokens, unsettled, err)
			}
			page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
			if err != nil || len(page.Matches) != 1 || page.Matches[0].Relation != "already_covered" {
				t.Fatal(page, err)
			}
		})
	}
}
