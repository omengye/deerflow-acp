package desktopconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

// Inheritance survives saves and default changes; named endpoints and credentials
// must follow the global default even when the session default uses another route.
func TestMaintenanceModelInheritanceSaveLaunchAndDefaultChanges(t *testing.T) {
	for _, inherited := range []any{nil, ""} {
		name := "null"
		if inherited != nil {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			options, doc := behaviorDocument(t)
			keys := []string{"moderation_model_name", "evaluation_model_name", "generation_model_name"}
			for _, key := range keys {
				readObject(doc, "skill_evolution")[key] = inherited
			}
			readObject(doc, "runtime")["model_name"] = "second"
			var calls [2]atomic.Int32
			var routes [2]harness.ModelRoute
			for index, modelName := range []string{"first", "second"} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls[index].Add(1)
					if r.Header.Get("Authorization") != "Bearer "+modelName+"-private-key" {
						t.Error("inherited route used another model's credential")
					}
					var request struct {
						Model    string                     `json:"model"`
						Messages []struct{ Content string } `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "shared-id" || len(request.Messages) != 2 {
						t.Error("invalid review request", err)
						http.Error(w, "bad request", 400)
						return
					}
					var input map[string]any
					if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
						t.Error(err)
						return
					}
					answer, _ := json.Marshal(map[string]any{"base_version": input["base_version"], "candidate_version": input["candidate_version"], "passed": true, "reason": "Fixture review"})
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}})
				}))
				defer server.Close()
				routes[index] = harness.ModelRoute{Provider: "openai", Model: "shared-id", BaseURL: server.URL + "/v1", APIKey: modelName + "-private-key"}
				model := modelByName(t, doc, modelName)
				model["provider"], model["model"], model["base_url"], model["api_key"] = routes[index].Provider, routes[index].Model, routes[index].BaseURL, routes[index].APIKey
			}
			var firstBehaviorIdentity string
			for round := 0; round < 3; round++ {
				wantRoute := 0
				if round > 0 {
					doc["default_model"] = "second"
					wantRoute = 1
				}
				if round == 2 {
					for _, key := range keys {
						readObject(doc, "skill_evolution")[key] = "first"
					}
					wantRoute = 0
				}
				if run(t, options, "validate", doc)["valid"] != true {
					t.Fatal("inherited configuration failed validation")
				}
				doc = run(t, options, "save", doc)
				for _, key := range keys {
					want := inherited
					if round == 2 {
						want = "first"
					}
					if !reflect.DeepEqual(readObject(doc, "skill_evolution")[key], want) {
						t.Fatalf("%s inheritance or override was replaced on save", key)
					}
				}
				var cfg deerflow.Config
				connections := 0
				if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
					t.Fatal(err)
				}
				if cfg.BaseURL != routes[1].BaseURL || cfg.SkillGeneration == nil || cfg.SkillGeneration.Model.Route != routes[wantRoute] || cfg.SkillGeneration.Budget.MaxDailyModelCalls != 3 {
					t.Fatal("maintenance inherited the session override, lost its route or changed its budget")
				}
				if cfg.SkillBehaviorEvaluator == nil {
					t.Fatal("inherited behavior evaluator was not configured")
				}
				identity := cfg.SkillBehaviorEvaluator.Identity()
				if round == 0 {
					firstBehaviorIdentity = identity
				} else if (identity == firstBehaviorIdentity) != (round == 2) {
					t.Fatal("behavior evaluator did not follow the default or explicit override")
				}
				before := [2]int32{calls[0].Load(), calls[1].Load()}
				report, err := cfg.SkillValidator.Validate(context.Background(), harness.SkillValidationInput{Proposal: harness.SkillProposal{Action: "create", SkillName: "research", CandidateVersion: "candidate"}, CandidateFiles: map[string][]byte{"SKILL.md": []byte("Fixture content")}})
				if err != nil || report.Security != "passed" {
					t.Fatalf("inherited reviews failed: %+v %v", report, err)
				}
				for index := range calls {
					want := int32(0)
					if index == wantRoute {
						want = 2
					}
					if calls[index].Load()-before[index] != want {
						t.Fatal("security and quality reviews did not use the resolved route")
					}
				}
			}
		})
	}
}
