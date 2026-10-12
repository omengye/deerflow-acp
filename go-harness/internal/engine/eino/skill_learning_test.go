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

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSkillLearningExtractorIsolatedBudgetedAndStrictJSON(t *testing.T) {
	for _, mode := range []string{"valid", "duplicate-field", "unknown-field", "wrong-episode", "null", "truncated", "provider-failure"} {
		t.Run(mode, func(t *testing.T) {
			db, err := sqlite.Open(filepath.Join(t.TempDir(), "learn.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			queue, err := skillmaintenance.New(context.Background(), db.DB(), skillmaintenance.Policy{MaxDailyModelCalls: 3, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1})
			if err != nil {
				t.Fatal(err)
			}
			job, err := queue.Enqueue(context.Background(), skillmaintenance.Request{Kind: skillmaintenance.ExperienceExtract, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: "extract", Reference: "lex-fixture", InputDigest: strings.Repeat("b", 64)})
			if err != nil {
				t.Fatal(err)
			}
			claim, ok, err := queue.ClaimKinds(context.Background(), []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract})
			if err != nil || !ok {
				t.Fatal(err, ok)
			}
			ctx := skillmaintenance.WithBroker(context.Background(), queue.Broker(claim))
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "provider-failure" {
					http.Error(w, "PRIVATE_PROVIDER_ERROR", 503)
					return
				}
				var req struct {
					Messages  []struct{ Role, Content string }
					Tools     []any
					MaxTokens int `json:"max_completion_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if len(req.Messages) != 2 || len(req.Tools) != 0 || req.MaxTokens != 512 {
					t.Errorf("invalid extraction model surface %+v", req)
				}
				if len(req.Messages) == 2 && (!strings.Contains(req.Messages[0].Content, "cite the earlier accepted user inputs being corrected") || !strings.Contains(req.Messages[0].Content, "Do not replace a later unverified issue with an earlier success")) {
					t.Error("extractor lost source-covered correction evidence contract")
				}
				answer := `{"episode_id":"episode-1","experiences":[{"kind":"capability_gap","goal":"Inspect JSON","applicability":"PowerShell","problem":"Partial reads","resolution":"Use full file text","steps":["Read complete text"],"verification":"Not yet verified","outcome":"unresolved","references":["input/one"]}]}`
				finish := "stop"
				switch mode {
				case "duplicate-field":
					answer = strings.Replace(answer, `"goal":"Inspect JSON"`, `"goal":"Inspect JSON","goal":"spoof"`, 1)
				case "unknown-field":
					answer = strings.Replace(answer, `"kind":"capability_gap"`, `"kind":"capability_gap","operator_confirmed":true`, 1)
				case "wrong-episode":
					answer = strings.Replace(answer, "episode-1", "foreign", 1)
				case "null":
					answer = `{"episode_id":"episode-1","experiences":null}`
				case "truncated":
					finish = "length"
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
			}))
			defer server.Close()
			extractor, err := NewSkillExperienceExtractor(ctx, harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL, APIKey: "PRIVATE_API_KEY"}, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			input := harness.SkillLearningInput{EpisodeID: "episode-1", References: []harness.SkillLearningReference{{ID: "input/one", RunID: "one", Kind: "user", Text: "Inspect complete JSON"}}}
			if _, err = extractor.Extract(context.Background(), input); err == nil || calls.Load() != 0 {
				t.Fatal("unbudgeted call")
			}
			result, err := extractor.Extract(ctx, input)
			if (err == nil) != (mode == "valid") || mode == "valid" && len(result) != 1 {
				t.Fatalf("result %+v %v", result, err)
			}
			if calls.Load() != 1 {
				t.Fatal("provider retried outside host queue")
			}
			if err != nil && (strings.Contains(err.Error(), "PRIVATE_") || strings.Contains(err.Error(), server.URL)) {
				t.Fatal("error leaks model details")
			}
			receipts, err := queue.Receipts(context.Background(), job.ID, 0, 10)
			if err != nil || len(receipts) != 1 || receipts[0].Stage != "experience_extract" || receipts[0].State != "settled" {
				t.Fatalf("receipt %+v %v", receipts, err)
			}
			if mode == "provider-failure" && (!receipts[0].Estimated || receipts[0].ChargedTokens != receipts[0].ReservedTokens) {
				t.Fatal("failed call refunded")
			}
		})
	}
}

func TestSkillLearningTextProjectionRetainsAgentAttribution(t *testing.T) {
	// The public transcript retains child output, while extraction can select
	// the final lead answer instead of treating child text as a user reply.
	for _, name := range []string{"deerflow", "researcher"} {
		var events []harness.RunEvent
		sink := &eventSink{request: harness.RunRequest{Session: harness.Session{ID: "s"}, RunID: "r"}, callback: func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }}
		_, err := consumeMessage(context.Background(), &adk.MessageVariant{Role: schema.Assistant, Message: schema.AssistantMessage("visible", nil)}, sink, &harness.RunResult{}, name)
		if err != nil || len(events) != 1 || events[0].AgentName != name {
			t.Fatalf("lost agent provenance %+v %v", events, err)
		}
	}
}
