package deerflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type skillRetryFixture struct {
	t       *testing.T
	id      string
	failure *harness.SkillValidationFailure
	reviews atomic.Int32
}

func (f *skillRetryFixture) Identity() string {
	if f.id != "" {
		return f.id
	}
	return "skill-retry-fixture-v1"
}
func (f *skillRetryFixture) Validate(_ context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
	f.reviews.Add(1)
	security := "unavailable"
	if f.failure.Code == "security_rejected" {
		security = "failed"
	}
	return harness.SkillValidationReport{ValidatorVersion: f.Identity(), BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: security, Behavior: "not_run"}, f.failure
}
func (f *skillRetryFixture) Generate(context.Context, harness.SkillGenerationInput) (harness.SkillGeneratedCandidate, error) {
	f.t.Error("review retry must not generate different content")
	return harness.SkillGeneratedCandidate{}, harness.ErrPermissionDenied
}
func (f *skillRetryFixture) Evaluate(context.Context, harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	f.t.Error("queued review must not execute synchronously")
	return harness.SkillBehaviorResult{}, harness.ErrPermissionDenied
}

func TestSkillManageReviewRetryOwnershipAndBounds(t *testing.T) {
	for _, mode := range []string{"owner", "legacy-owner", "foreign-session", "stale-state", "security-denied", "content-change", "missing-state", "maintenance-disabled", "classified-unknown", "configuration-corrected", "configuration-same", "configuration-unknown"} {
		t.Run(mode, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.SkillProposalSources = []string{"project"}
			cfg.PermissionMode = harness.PermissionModeOff
			fixture := &skillRetryFixture{t: t, failure: &harness.SkillValidationFailure{Code: "review_response_invalid", Stage: "security", DetailCode: "invalid_json", Retryable: true}}
			if mode == "security-denied" {
				fixture.failure = &harness.SkillValidationFailure{Code: "security_rejected", Stage: "security"}
			} else if mode == "legacy-owner" || mode == "classified-unknown" {
				fixture.failure = &harness.SkillValidationFailure{Code: "review_request_failed", Stage: "security"}
				if mode == "classified-unknown" {
					fixture.failure.DetailCode = "cause_unavailable"
				}
			} else if strings.HasPrefix(mode, "configuration-") {
				fixture.failure = &harness.SkillValidationFailure{Code: "review_request_failed", Stage: "security", DetailCode: "http_404"}
				if mode == "configuration-unknown" {
					fixture.failure.DetailCode = "cause_unavailable"
				}
			}
			cfg.SkillValidator = fixture
			var mu sync.Mutex
			var arguments string
			var observed map[string]any
			call := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Stream   bool
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				call++
				if call == 1 {
					skillToolResponse(w, request.Stream, "skill_manage", arguments, "")
					return
				}
				for _, message := range request.Messages {
					if message.Role == "tool" {
						var value map[string]any
						content := message.Content
						if start, end := strings.Index(content, "{"), strings.LastIndex(content, "}"); start >= 0 && end >= start {
							content = content[start : end+1]
						}
						if json.Unmarshal([]byte(content), &value) == nil {
							observed = value
						} else {
							t.Errorf("unrecognized tool feedback: %s", message.Content)
						}
					}
				}
				skillToolResponse(w, request.Stream, "", "", "Review status inspected.")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			owner, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			identity, _ := json.Marshal([]string{"session", owner.ID})
			author := fmt.Sprintf("model/%x", sha256.Sum256(identity))
			head, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil {
				t.Fatal(err)
			}
			find, replace := "BODY_ONLY_AFTER_LOAD", "VERIFY_ALL_SOURCES"
			p, err := c.SubmitSkillProposal(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "research", Base: head, Action: "patch", Find: &find, Replace: &replace, RequestKey: "retry-fixture", Reason: "Verify the sources before summarizing", Author: author, Origin: "manual_agent"})
			if err != nil || p.Status != "failed" {
				t.Fatalf("failed proposal fixture: %+v %v", p, err)
			}
			if mode == "configuration-corrected" || mode == "configuration-unknown" {
				fixture.id = "skill-retry-corrected-config"
			}
			// Configure the native queue before any workers start. The tool must
			// only enqueue; no live or paid model is involved in this test.
			if mode != "maintenance-disabled" {
				if err = c.evolution.ConfigureGeneration(ctx, fixture, harness.SkillMaintenanceBudget{MaxDailyModelCalls: 10, MaxDailyTokens: 100000, MaxJobSeconds: 30, Workers: 1}, []string{"project"}, false); err != nil {
					t.Fatal(err)
				}
				suite := harness.SkillBehaviorSuite{Version: "skill-behavior-v1", ID: "retry-suite", SourceID: "project", Name: "research", Cases: []harness.SkillBehaviorCase{{ID: "verify", Kind: "targeted", Prompt: "Verify sources", Repetitions: 1, Assertions: []harness.SkillBehaviorAssertion{{ID: "answer", Kind: "output_contains", Expected: "verified"}}}}}
				if err = c.evolution.ConfigureBehavior(fixture, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{suite}}); err != nil {
					t.Fatal(err)
				}
			}
			sessionID := owner.ID
			if mode == "foreign-session" {
				foreign, e := c.NewSession(ctx, workspace)
				if e != nil {
					t.Fatal(e)
				}
				sessionID = foreign.ID
			}
			run := func(input map[string]any) map[string]any {
				t.Helper()
				data, _ := json.Marshal(input)
				mu.Lock()
				arguments, observed, call = string(data), nil, 0
				mu.Unlock()
				if _, err := c.Run(ctx, sessionID, []harness.Content{{Type: "text", Text: "Inspect and recover my candidate review"}}, nil, nil); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				if observed == nil || call != 2 {
					t.Fatalf("tool result missing: %+v calls=%d", observed, call)
				}
				return observed
			}
			input := map[string]any{"action": "review_retry", "proposal_id": p.ID, "expected_state_version": p.StateVersion}
			switch mode {
			case "stale-state":
				input["expected_state_version"] = p.StateVersion + 1
			case "missing-state":
				delete(input, "expected_state_version")
			case "content-change":
				input["reason"] = "Weaken the original requirement"
			}
			if mode == "configuration-corrected" {
				got := run(map[string]any{"action": "get", "proposal_id": p.ID})
				if got["review_retry_available"] != true || !strings.Contains(got["next_action"].(string), "configuration changed") {
					t.Fatalf("corrected configuration recovery was not advertised: %+v", got)
				}
			}
			result := run(input)
			if mode == "owner" || mode == "legacy-owner" || mode == "configuration-corrected" {
				nextID, _ := result["proposal_id"].(string)
				if result["ok"] != true || nextID == "" || nextID == p.ID || result["review_retry_of"] != p.ID || result["status"] != "validating" || result["review_retry_available"] != false || result["approval_recorded"] != false {
					t.Fatalf("incorrect retry result: %+v", result)
				}
				if duplicate := run(input); duplicate["proposal_id"] != nextID {
					t.Fatalf("duplicate retry created another candidate: %+v", duplicate)
				}
				if got := run(map[string]any{"action": "get", "proposal_id": nextID}); got["proposal_id"] != nextID || got["ok"] != true {
					t.Fatalf("retry lost session ownership: %+v", got)
				}
				next, e := c.evolution.Get(ctx, nextID)
				if e != nil || next.Author != p.Author || next.Target != p.Target || next.CandidateVersion != p.CandidateVersion || next.Reason != p.Reason || next.Base != p.Base || next.Approval != nil {
					t.Fatalf("retry changed immutable candidate or approval: %+v %v", next, e)
				}
				if again := run(map[string]any{"action": "review_retry", "proposal_id": nextID, "expected_state_version": next.StateVersion}); again["code"] != "review_retry_unavailable" {
					t.Fatalf("successor retry accepted: %+v", again)
				}
			} else {
				codes := map[string]string{"foreign-session": "not_found", "stale-state": "conflict", "security-denied": "review_retry_unavailable", "content-change": "invalid_request", "missing-state": "invalid_request", "maintenance-disabled": "review_retry_unavailable", "classified-unknown": "review_retry_unavailable", "configuration-same": "review_retry_unavailable", "configuration-unknown": "review_retry_unavailable"}
				if result["ok"] != false || result["code"] != codes[mode] || result["proposal_id"] != nil {
					t.Fatalf("unsafe retry admitted or foreign proposal exposed: %+v", result)
				}
			}
			page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 10})
			want := 1
			if mode == "owner" || mode == "legacy-owner" || mode == "configuration-corrected" {
				want = 2
			}
			if err != nil || len(page.Proposals) != want || fixture.reviews.Load() != 1 {
				t.Fatalf("review re-executed or duplicated: count=%d reviews=%d err=%v", len(page.Proposals), fixture.reviews.Load(), err)
			}
			unchanged, err := c.evolution.Get(ctx, p.ID)
			current, headErr := c.GetSkillHead(ctx, "project", "research")
			if err != nil || headErr != nil || unchanged.Status != p.Status || unchanged.StateVersion != p.StateVersion || current != head {
				t.Fatal("retry changed original proposal or installed skill", err, headErr)
			}
		})
	}
}
