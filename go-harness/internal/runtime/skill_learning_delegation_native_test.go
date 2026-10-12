package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type learningDelegationNativeExtractor struct {
	harness.SkillExperienceExtractor
	before func()
}

func (e *learningDelegationNativeExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	if e.before != nil {
		broker, ok := skillmaintenance.BrokerFromContext(ctx)
		if !ok {
			return nil, harness.ErrPermissionDenied
		}
		ctx = skillmaintenance.WithBroker(ctx, &learningInFlightBroker{Broker: broker, before: e.before})
		e.before = nil
	}
	return e.SkillExperienceExtractor.Extract(ctx, input)
}

func TestSkillLearningDelegatedExecutionNativeExtractorAdmissionAndView(t *testing.T) {
	for _, point := range []string{"valid", "reservation-change", "response-change"} {
		t.Run(point, func(t *testing.T) {
			s, x, _ := newLearningTest(t, true)
			x = learningDelegationSession(t, s, x)
			ctx := context.Background()
			var calls atomic.Int32
			var fixture *backgroundHostFixture
			change := func() {
				if _, err := s.Store.Append(ctx, harness.RunEvent{SessionID: fixture.binding.ChildSessionID, RunID: fixture.binding.TaskID, Kind: "text_delta", AgentName: "deerflow", Text: "Changed original delegated frontier"}); err != nil {
					t.Error(err)
				}
			}
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request struct {
					Messages []struct{ Role, Content string }
					Tools    []any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 || len(request.Tools) != 0 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" {
					t.Error("delegated extraction did not use isolated native input", err)
					http.Error(w, "invalid fixture request", 400)
					return
				}
				if !strings.Contains(request.Messages[0].Content, "delegated runs") || !strings.Contains(request.Messages[0].Content, "never user corrections or independent votes") || strings.Contains(request.Messages[1].Content, "PRIVATE_") {
					t.Error("native delegated input lost policy or leaked private text")
				}
				var input harness.SkillLearningInput
				if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
					t.Error(err)
				}
				users, observations, refs := 0, 0, []string{}
				for _, ref := range input.References {
					if ref.Kind == "user" {
						users++
						refs = append(refs, ref.ID)
					}
					if ref.Kind == "tool_observation" && ref.RunID == fixture.binding.TaskID && ref.ToolName == "read_file" && ref.State == "completed" && !ref.Truncated {
						observations++
						refs = append(refs, ref.ID)
					}
				}
				if users != 1 || observations != 1 {
					t.Error("native extractor lost accepted-user/delegated-receipt boundary", users, observations)
				}
				if point == "response-change" {
					change()
				}
				answer, _ := json.Marshal(map[string]any{"episode_id": input.EpisodeID, "experiences": []any{map[string]any{
					"kind": "proven_recipe", "goal": "Inspect complete local input", "applicability": "Local JSON input", "problem": "", "resolution": "Read and verify the complete document",
					"steps": []string{"Read complete input", "Check required fields"}, "verification": "Host local receipt completed; effectiveness remains unassessed", "outcome": "observed_execution", "references": refs,
				}}})
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
			}))
			defer server.Close()
			native, err := einoengine.NewSkillExperienceExtractor(ctx, harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture-only"}, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			extractor := &learningDelegationNativeExtractor{SkillExperienceExtractor: native}
			if point == "reservation-change" {
				extractor.before = change
			}
			learningDelegationRestart(t, s, learningTestOptions(), extractor, nil, nil)
			store := learningDelegationNativeUnconfigured(t, s)
			parent := learningTestRun(t, s, x, "Perform a real local check through the delegated agent")
			fixture = learningDelegationCreate(t, s, store, parent, "native")
			learningTestFinish(t, s, parent, "end_turn")
			learningDelegationComplete(t, fixture, "read_file")
			learningTestQueue(t, s)
			matchingRun(t, s)
			var job string
			if err := s.Store.db.QueryRow(`SELECT job_id FROM df_learning_extractions`).Scan(&job); err != nil {
				t.Fatal(err)
			}
			got, err := skillmaintenance.ReadJob(ctx, s.Store.db, job)
			state, code, experiences, paid := "completed", "", 1, 1
			if point != "valid" {
				state, code, experiences = "failed", "evidence_changed", 0
			}
			if point == "reservation-change" {
				paid = 0
			}
			if err != nil || got.State != state || got.ErrorCode != code || got.Attempts != 1 || learningCount(t, s.Store, "df_learning_experiences") != experiences || int(calls.Load()) != paid {
				t.Fatal("native extractor bypassed delegated snapshot admission", got, calls.Load(), err)
			}
			var receipts, tokens, unsettled int
			if err = s.Store.db.QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0),COALESCE(sum(state<>'settled'),0) FROM df_skill_maintenance_calls WHERE job_id=?`, job).Scan(&receipts, &tokens, &unsettled); err != nil || receipts != paid || tokens != 15*paid || unsettled != 0 {
				t.Fatal("native root rejection refunded or admitted paid work", receipts, tokens, unsettled, err)
			}
		})
	}
}
