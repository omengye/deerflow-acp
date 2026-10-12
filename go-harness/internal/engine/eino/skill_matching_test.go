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

func TestSkillLearningMatchingEinoIsolatedStrictAndBudgeted(t *testing.T) {
	for _, mode := range []string{"valid", "trigger", "covered", "new", "ambiguous", "duplicate", "unknown", "foreign", "wrong-pattern", "wrong-revision", "null", "incomplete", "truncated", "provider-failure"} {
		t.Run(mode, func(t *testing.T) {
			db, err := sqlite.Open(filepath.Join(t.TempDir(), "matching.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			queue, err := skillmaintenance.New(context.Background(), db.DB(), skillmaintenance.Policy{MaxDailyModelCalls: 3, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1})
			if err != nil {
				t.Fatal(err)
			}
			job, err := queue.Enqueue(context.Background(), skillmaintenance.Request{Kind: skillmaintenance.SkillMatch, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: "match", Reference: "matching", InputDigest: strings.Repeat("b", 64)})
			if err != nil {
				t.Fatal(err)
			}
			claim, ok, err := queue.ClaimKinds(context.Background(), []skillmaintenance.JobKind{skillmaintenance.SkillMatch})
			if err != nil || !ok {
				t.Fatal(err, ok)
			}
			ctx := skillmaintenance.WithBroker(context.Background(), queue.Broker(claim))
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "provider-failure" {
					http.Error(w, "PRIVATE_MATCH_PROVIDER", 503)
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
					t.Error("invalid matching model surface", request)
				}
				answer := `{"pattern_id":"pattern","revision":2,"relation":"body_gap","skill_id":"allowed","reason":"Missing a concrete instruction","suggested_name":""}`
				finish := "stop"
				switch mode {
				case "trigger":
					answer = strings.Replace(answer, "body_gap", "trigger_mismatch", 1)
				case "covered":
					answer = strings.Replace(answer, "body_gap", "already_covered", 1)
				case "new", "incomplete":
					answer = strings.Replace(answer, "body_gap", "new_capability", 1)
					answer = strings.Replace(answer, `"allowed"`, `""`, 1)
					answer = strings.Replace(answer, `"suggested_name":""`, `"suggested_name":"structured-json-audit"`, 1)
				case "ambiguous":
					answer = strings.Replace(answer, "body_gap", "ambiguous", 1)
					answer = strings.Replace(answer, `"allowed"`, `""`, 1)
				case "duplicate":
					answer = strings.Replace(answer, `"relation":"body_gap"`, `"relation":"body_gap","relation":"ambiguous"`, 1)
				case "unknown":
					answer = strings.Replace(answer, `"pattern_id"`, `"target_source":"foreign","pattern_id"`, 1)
				case "foreign":
					answer = strings.Replace(answer, "allowed", "foreign", 1)
				case "wrong-pattern":
					answer = strings.Replace(answer, `"pattern"`, `"foreign"`, 1)
				case "wrong-revision":
					answer = strings.Replace(answer, `"revision":2`, `"revision":3`, 1)
				case "null":
					answer = strings.Replace(answer, `"allowed"`, `null`, 1)
				case "truncated":
					finish = "length"
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
			}))
			defer server.Close()
			matcher, err := NewSkillExperienceMatcher(ctx, harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL, APIKey: "PRIVATE_API_KEY"}, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			input := harness.SkillExperienceMatchInput{PatternID: "pattern", Revision: 2, InventoryComplete: mode != "incomplete", Skills: []harness.SkillExperienceMatchChoice{{ID: "allowed", Name: "sample", Description: "Inspect JSON", Body: "Validate required fields"}}}
			if _, err = matcher.Match(context.Background(), input); err == nil || calls.Load() != 0 {
				t.Fatal("unbudgeted matching call")
			}
			result, err := matcher.Match(ctx, input)
			want := mode == "valid" || mode == "trigger" || mode == "covered" || mode == "new" || mode == "ambiguous"
			if (err == nil) != want || calls.Load() != 1 {
				t.Fatal(result, err, calls.Load())
			}
			if err != nil && (strings.Contains(err.Error(), "PRIVATE_") || strings.Contains(err.Error(), server.URL)) {
				t.Fatal("provider details leaked", err)
			}
			receipts, err := queue.Receipts(context.Background(), job.ID, 0, 10)
			if err != nil || len(receipts) != 1 || receipts[0].Stage != "skill_match" || receipts[0].State != "settled" {
				t.Fatal(receipts, err)
			}
			if mode == "provider-failure" && (!receipts[0].Estimated || receipts[0].ChargedTokens != receipts[0].ReservedTokens) {
				t.Fatal("failed matching call refunded")
			}
		})
	}
}

func TestSkillLearningMatchingEinoParserRejectsTypesCaseAndExtraJSON(t *testing.T) {
	input := harness.SkillExperienceMatchInput{PatternID: "pattern", Revision: 1, InventoryComplete: true, Skills: []harness.SkillExperienceMatchChoice{}}
	valid := `{"pattern_id":"pattern","revision":1,"relation":"ambiguous","skill_id":"","reason":"No clear match","suggested_name":""}`
	for _, text := range []string{
		strings.Replace(valid, "pattern_id", "Pattern_ID", 1),
		strings.Replace(valid, `"revision":1`, `"revision":1.0`, 1),
		strings.Replace(valid, `"revision":1`, `"revision":"1"`, 1),
		strings.Replace(valid, "ambiguous", "publish", 1),
		strings.Replace(valid, `"skill_id":""`, `"skill_id":"foreign"`, 1),
		strings.Replace(valid, `"No clear match"`, `""`, 1),
		strings.Replace(valid, `,"suggested_name":""`, ``, 1),
		strings.Replace(valid, `"suggested_name":""`, `"suggested_name":null`, 1),
		strings.Replace(valid, `"suggested_name":""`, `"suggested_name":"structured-json-audit"`, 1),
		strings.Replace(valid, `"suggested_name":""`, `"suggested_name":"","suggested_name":""`, 1),
		valid + " {}",
		"```json\n" + valid + "\n```",
	} {
		if _, err := parseMatch(text, input); err == nil {
			t.Fatal("invalid matching response accepted", text)
		}
	}
}

func TestSkillLearningMatchingEinoCapabilityNameCannotChooseSourceOrPath(t *testing.T) {
	input := harness.SkillExperienceMatchInput{PatternID: "pattern", Revision: 1, InventoryComplete: true, Skills: []harness.SkillExperienceMatchChoice{}}
	valid := `{"pattern_id":"pattern","revision":1,"relation":"new_capability","skill_id":"","reason":"A distinct reusable procedure","suggested_name":"structured-json-audit"}`
	got, err := parseMatch(valid, input)
	if err != nil || got.SuggestedName != "structured-json-audit" || got.SkillID != "" {
		t.Fatal(got, err)
	}
	for _, name := range []string{"", "config", "../other-skill", "config-123", "workflow-0123456789abcdef", "New-Skill"} {
		if _, err := parseMatch(strings.Replace(valid, "structured-json-audit", name, 1), input); err == nil {
			t.Fatal("invalid capability label accepted", name)
		}
	}
	if _, err := parseMatch(strings.Replace(valid, `"pattern_id"`, `"source_id":"foreign","pattern_id"`, 1), input); err == nil {
		t.Fatal("matching chose a new capability source")
	}
}
