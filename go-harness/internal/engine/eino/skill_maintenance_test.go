package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func maintenanceReviewFixture(t *testing.T, calls int64) (*skillmaintenance.Store, *sqlite.Store, skillmaintenance.Claim, context.Context) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "maintenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := skillmaintenance.New(context.Background(), db.DB(), skillmaintenance.Policy{MaxDailyModelCalls: calls, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Enqueue(context.Background(), skillmaintenance.Request{SourceIdentity: strings.Repeat("a", 64), Name: "research", Key: "review", Reference: "proposal-fixture", InputDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.Claim(context.Background())
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	return s, db, c, skillmaintenance.WithBroker(context.Background(), s.Broker(c))
}

func TestSkillMaintenanceReviewChargesBothStages(t *testing.T) {
	for _, mode := range []string{"complete", "one-call-budget", "invalid-answer", "provider-failure", "missing-usage", "partial-usage", "inconsistent-usage", "underreported-usage", "settle-failure", "reserve-failure"} {
		t.Run(mode, func(t *testing.T) {
			budget := int64(3)
			if mode == "one-call-budget" {
				budget = 1
			}
			s, db, c, ctx := maintenanceReviewFixture(t, budget)
			if mode == "settle-failure" {
				if _, e := db.DB().Exec(`CREATE TRIGGER fail_review_settle BEFORE UPDATE ON df_skill_maintenance_calls BEGIN SELECT RAISE(ABORT,'private-storage'); END`); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "reserve-failure" {
				if _, e := db.DB().Exec(`CREATE TRIGGER fail_review_reserve BEFORE INSERT ON df_skill_maintenance_calls BEGIN SELECT RAISE(ABORT,'private-storage'); END`); e != nil {
					t.Fatal(e)
				}
			}
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "provider-failure" {
					http.Error(w, "private-provider", 503)
					return
				}
				var req struct {
					MaxCompletionTokens int   `json:"max_completion_tokens"`
					Tools               []any `json:"tools"`
				}
				if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
					t.Error(e)
				}
				if req.MaxCompletionTokens != 512 || len(req.Tools) != 0 {
					t.Errorf("request %+v", req)
				}
				answer := `{"base_version":"base-digest","candidate_version":"candidate-digest","passed":true,"reason":"fixture"}`
				if mode == "invalid-answer" {
					answer = `{"not-a-review":true}`
				}
				body := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}}
				if mode != "missing-usage" {
					body["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
				}
				if mode == "partial-usage" {
					body["usage"] = map[string]any{"completion_tokens": 5}
				}
				if mode == "inconsistent-usage" {
					body["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 30}
				}
				if mode == "underreported-usage" {
					body["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 5}
				}
				_ = modelstreamtest.WriteCompletion(w, body)
			}))
			defer server.Close()
			route := harness.ModelRoute{Model: "reviewer", BaseURL: server.URL, APIKey: "private-key"}
			validator, e := NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route, Evaluation: route, MaxOutputTokens: 512})
			if e != nil {
				t.Fatal(e)
			}
			report, e := validator.Validate(ctx, reviewInput())
			wantCalls := int32(1)
			success := mode == "complete" || mode == "missing-usage" || mode == "partial-usage" || mode == "inconsistent-usage" || mode == "underreported-usage"
			if success {
				wantCalls = 2
			}
			if mode == "reserve-failure" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("calls %d want %d", calls.Load(), wantCalls)
			}
			if success && (e != nil || report.Security != "passed" || len(report.ModelChecks) != 2) {
				t.Fatalf("report %+v %v", report, e)
			}
			if !success && !errors.Is(e, harness.ErrSkillValidation) {
				t.Fatalf("failure %v", e)
			}
			if e != nil && (strings.Contains(e.Error(), "private-") || strings.Contains(e.Error(), server.URL)) {
				t.Fatalf("leaked error %v", e)
			}
			receipts, e := s.Receipts(context.Background(), c.Job.ID, 0, 100)
			if e != nil || len(receipts) != int(wantCalls) {
				t.Fatalf("receipts %+v %v", receipts, e)
			}
			for i, r := range receipts {
				stage := "security"
				if i == 1 {
					stage = "quality"
				}
				if r.Stage != stage || len(r.ModelIdentity) != 64 || len(r.InputDigest) != 64 {
					t.Fatal(r)
				}
				estimated := mode == "provider-failure" || mode == "missing-usage" || mode == "partial-usage" || mode == "inconsistent-usage" || mode == "underreported-usage" || mode == "settle-failure"
				if r.Estimated != estimated || estimated && r.ChargedTokens != r.ReservedTokens || !estimated && r.ChargedTokens != 15 {
					t.Fatalf("charge %+v", r)
				}
			}
		})
	}
}

func TestSkillMaintenanceProviderRetriesAreHostOwned(t *testing.T) {
	for _, provider := range []string{"openai", "claude"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				fmt.Fprint(w, `{"error":{"type":"overloaded_error","message":"private-model-failure"}}`)
			}))
			defer server.Close()
			s, _, c, ctx := maintenanceReviewFixture(t, 10)
			route := harness.ModelRoute{Provider: provider, Model: "reviewer", BaseURL: server.URL, APIKey: "fixture"}
			validator, e := NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route, Evaluation: route, Timeout: 5 * time.Second})
			if e != nil {
				t.Fatal(e)
			}
			_, e = validator.Validate(ctx, reviewInput())
			if !errors.Is(e, harness.ErrSkillValidation) || calls.Load() != 1 {
				t.Fatalf("hidden %s retries: calls=%d err=%v", provider, calls.Load(), e)
			}
			r, e := s.Receipts(context.Background(), c.Job.ID, 0, 100)
			if e != nil || len(r) != 1 || !r[0].Estimated || r[0].State != "settled" {
				t.Fatalf("receipts %+v %v", r, e)
			}
		})
	}
}
