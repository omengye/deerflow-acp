package eino

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func TestSkillLearningNativeAdaptersPreserveTemporarySourceAdmission(t *testing.T) {
	for _, stage := range []string{"extract", "consolidate", "match", "generate", "content", "behavior", "trigger"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			db, err := sqlite.Open(filepath.Join(t.TempDir(), "source-active.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			queue, err := skillmaintenance.NewWithHooks(ctx, db.DB(), skillmaintenance.Policy{MaxDailyModelCalls: 5, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}, skillmaintenance.Hooks{Admission: func(context.Context, *sql.Tx, skillmaintenance.Job) error {
				return fmt.Errorf("private-provider-and-source-details: %w", errors.Join(harness.ErrPermissionDenied, skillmaintenance.ErrSourceActive))
			}})
			if err != nil {
				t.Fatal(err)
			}
			request := skillmaintenance.Request{SourceIdentity: strings.Repeat("a", 64), Name: "research", Key: "active", Reference: "input", InputDigest: strings.Repeat("b", 64)}
			switch stage {
			case "extract", "consolidate", "match":
				request.SourceIdentity, request.Name = "", ""
				request.OwnerKind, request.OwnerKey = skillmaintenance.WorkspaceOwner, strings.Repeat("c", 64)
				request.Kind = map[string]skillmaintenance.JobKind{"extract": skillmaintenance.ExperienceExtract, "consolidate": skillmaintenance.ExperienceConsolidate, "match": skillmaintenance.SkillMatch}[stage]
			default:
				request.Kind = skillmaintenance.SkillGeneration
			}
			job, err := queue.Enqueue(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			claim, ok, err := queue.ClaimKinds(ctx, []skillmaintenance.JobKind{request.Kind})
			if err != nil || !ok {
				t.Fatal(ok, err)
			}
			ctx = skillmaintenance.WithBroker(ctx, queue.Broker(claim))
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "provider must not be called", 500)
			}))
			defer server.Close()
			route := harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "fixture-only"}
			config := harness.SkillGenerationModelConfig{Route: route, MaxOutputTokens: 512}
			switch stage {
			case "extract":
				adapter, buildErr := NewSkillExperienceExtractor(ctx, config)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				_, err = adapter.Extract(ctx, harness.SkillLearningInput{EpisodeID: "episode", References: []harness.SkillLearningReference{{ID: "input/one", Kind: "user", Text: "Inspect JSON"}}})
			case "consolidate":
				adapter, buildErr := NewSkillExperienceConsolidator(ctx, config)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				_, err = adapter.Consolidate(ctx, harness.SkillExperienceConsolidationInput{Seed: harness.SkillExperienceConsolidationSeed{ID: "seed", EpisodeID: "episode"}, Patterns: []harness.SkillExperienceConsolidationChoice{}})
			case "match":
				adapter, buildErr := NewSkillExperienceMatcher(ctx, config)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				_, err = adapter.Match(ctx, harness.SkillExperienceMatchInput{PatternID: "pattern", Revision: 1, InventoryComplete: true, Skills: []harness.SkillExperienceMatchChoice{}})
			case "generate":
				adapter, buildErr := NewSkillCandidateGenerator(ctx, config)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				_, err = adapter.Generate(ctx, generationInput())
			case "content":
				adapter, buildErr := NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route, Evaluation: route, MaxOutputTokens: 512})
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				_, err = adapter.Validate(ctx, reviewInput())
			case "behavior", "trigger":
				adapter, buildErr := NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: route, MaxOutputTokens: 512})
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				input := behaviorInput()
				if stage == "trigger" {
					input = triggerBehaviorInput("trigger_positive")
				}
				_, err = adapter.Evaluate(ctx, input)
			}
			if !errors.Is(err, skillmaintenance.ErrSourceActive) || !errors.Is(err, harness.ErrSkillValidation) || strings.Contains(err.Error(), "private-") || calls.Load() != 0 {
				t.Fatal("native adapter lost temporary cause, leaked diagnostics or called an unauthorized provider", err, calls.Load())
			}
			receipts, err := queue.Receipts(ctx, job.ID, 0, 10)
			if err != nil || len(receipts) != 0 {
				t.Fatal("denied admission consumed model budget", receipts, err)
			}
		})
	}
}
