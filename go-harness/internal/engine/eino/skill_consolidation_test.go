package eino

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSkillLearningConsolidatorIsolatedBudgetedStrictAndShortlistBound(t *testing.T) {
	for _, mode := range []string{"valid", "distinct", "conflicting", "ambiguous", "duplicate", "unknown", "wrong-seed", "foreign-target", "null", "truncated", "provider-failure"} {
		t.Run(mode, func(t *testing.T) {
			db, err := sqlite.Open(filepath.Join(t.TempDir(), "consolidation.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			queue, err := skillmaintenance.New(context.Background(), db.DB(), skillmaintenance.Policy{MaxDailyModelCalls: 3, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1})
			if err != nil {
				t.Fatal(err)
			}
			job, err := queue.Enqueue(context.Background(), skillmaintenance.Request{Kind: skillmaintenance.ExperienceConsolidate, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: "consolidate", Reference: "consolidation", InputDigest: strings.Repeat("b", 64)})
			if err != nil {
				t.Fatal(err)
			}
			claim, ok, err := queue.ClaimKinds(context.Background(), []skillmaintenance.JobKind{skillmaintenance.ExperienceConsolidate})
			if err != nil || !ok {
				t.Fatal(err, ok)
			}
			ctx := skillmaintenance.WithBroker(context.Background(), queue.Broker(claim))
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "provider-failure" {
					http.Error(w, "PRIVATE_CONSOLIDATION_PROVIDER", 503)
					return
				}
				var request struct {
					Messages  []struct{ Role, Content string }
					Tools     []any
					MaxTokens int `json:"max_completion_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if len(request.Messages) != 2 || len(request.Tools) != 0 || request.MaxTokens != 512 {
					t.Error("invalid consolidation surface", request)
				}
				answer := `{"seed_id":"seed-one","relation":"equivalent","pattern_id":"allowed","reason":"Same concrete procedure under matching environment"}`
				finish := "stop"
				switch mode {
				case "distinct", "ambiguous":
					answer = strings.Replace(answer, `"equivalent"`, `"`+mode+`"`, 1)
					answer = strings.Replace(answer, `"allowed"`, `""`, 1)
				case "conflicting":
					answer = strings.Replace(answer, `"equivalent"`, `"conflicting"`, 1)
				case "duplicate":
					answer = strings.Replace(answer, `"relation":"equivalent"`, `"relation":"equivalent","relation":"distinct"`, 1)
				case "unknown":
					answer = strings.Replace(answer, `"seed_id"`, `"operator_confirmed":true,"seed_id"`, 1)
				case "wrong-seed":
					answer = strings.Replace(answer, "seed-one", "foreign", 1)
				case "foreign-target":
					answer = strings.Replace(answer, "allowed", "foreign", 1)
				case "null":
					answer = strings.Replace(answer, `"allowed"`, "null", 1)
				case "truncated":
					finish = "length"
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
			}))
			defer server.Close()
			consolidator, err := NewSkillExperienceConsolidator(ctx, harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL, APIKey: "PRIVATE_API_KEY"}, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			input := harness.SkillExperienceConsolidationInput{Seed: harness.SkillExperienceConsolidationSeed{ID: "seed-one", EpisodeID: "episode"}, Patterns: []harness.SkillExperienceConsolidationChoice{{ID: "allowed", Revision: 2}}}
			if _, err = consolidator.Consolidate(context.Background(), input); err == nil || calls.Load() != 0 {
				t.Fatal("unbudgeted provider call")
			}
			result, err := consolidator.Consolidate(ctx, input)
			want := mode == "valid" || mode == "distinct" || mode == "conflicting" || mode == "ambiguous"
			if (err == nil) != want || calls.Load() != 1 {
				t.Fatal(result, err, calls.Load())
			}
			if err != nil && (strings.Contains(err.Error(), "PRIVATE_") || strings.Contains(err.Error(), server.URL)) {
				t.Fatal("provider details leaked", err)
			}
			receipts, err := queue.Receipts(context.Background(), job.ID, 0, 10)
			if err != nil || len(receipts) != 1 || receipts[0].Stage != "experience_consolidate" || receipts[0].State != "settled" {
				t.Fatal(receipts, err)
			}
			if mode == "provider-failure" && (!receipts[0].Estimated || receipts[0].ChargedTokens != receipts[0].ReservedTokens) {
				t.Fatal("failed consolidation call refunded")
			}
		})
	}
}

func TestSkillLearningConsolidatorParserRejectsCaseTypesAndExtraJSON(t *testing.T) {
	input := harness.SkillExperienceConsolidationInput{Seed: harness.SkillExperienceConsolidationSeed{ID: "seed"}, Patterns: []harness.SkillExperienceConsolidationChoice{}}
	valid := `{"seed_id":"seed","relation":"distinct","pattern_id":"","reason":"Different procedure"}`
	for _, text := range []string{
		strings.Replace(valid, "seed_id", "Seed_ID", 1),
		strings.Replace(valid, `"seed"`, "123", 1),
		strings.Replace(valid, `"distinct"`, `"publish"`, 1),
		strings.Replace(valid, `"pattern_id":""`, `"pattern_id":"foreign"`, 1),
		strings.Replace(valid, `"Different procedure"`, `""`, 1),
		valid + " {}",
		"```json\n" + valid + "\n```",
	} {
		if _, err := parseConsolidation(text, input); err == nil {
			t.Fatal("invalid response accepted", text)
		}
	}
}
