package eino

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func generationInput() skillmaintenance.GenerationInput {
	return skillmaintenance.GenerationInput{Target: harness.SkillTarget{SourceID: "project", SourceIdentity: strings.Repeat("a", 64), Name: "research", Scope: harness.SkillScopeWorkspace}, Base: harness.SkillHead{Version: strings.Repeat("b", 64), Exists: true}, Reason: "Add a source verification step", BaseFiles: map[string][]byte{"SKILL.md": []byte("---\nname: research\ndescription: Research a topic\n---\nSummarize sources.\n")}, Evidence: []skillmaintenance.Evidence{{Reference: "user-feedback/1", Summary: "The operator explicitly requested source verification."}}}
}

func TestSkillGenerationAndBothReviewsShareMaintenanceBudget(t *testing.T) {
	for _, allowed := range []int64{2, 3} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			s, _, c, ctx := maintenanceReviewFixture(t, allowed)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index := calls.Add(1)
				var req struct {
					Messages            []struct{ Role, Content string }
					Tools               []any
					MaxCompletionTokens int `json:"max_completion_tokens"`
				}
				if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
					t.Error(e)
				}
				if len(req.Messages) != 2 || len(req.Tools) != 0 || req.Messages[0].Role != "system" {
					t.Errorf("unexpected generation/review request %+v", req)
					http.Error(w, "bad request", 400)
					return
				}
				var payload map[string]any
				if e := json.Unmarshal([]byte(req.Messages[1].Content), &payload); e != nil {
					t.Error(e)
				}
				var answer []byte
				if index == 1 {
					if req.MaxCompletionTokens != 8192 {
						t.Errorf("generation cap %d", req.MaxCompletionTokens)
					}
					answer, _ = json.Marshal(map[string]any{"skill_name": payload["skill_name"], "base_version": payload["base_version"], "reason": "Verify citations before summarizing", "files": map[string]string{"SKILL.md": "---\nname: research\ndescription: Research a topic\n---\nVerify source claims, then summarize.\n"}})
				} else {
					if req.MaxCompletionTokens != 512 {
						t.Errorf("review cap %d", req.MaxCompletionTokens)
					}
					answer, _ = json.Marshal(map[string]any{"base_version": payload["base_version"], "candidate_version": payload["candidate_version"], "passed": true, "reason": "Scoped content change"})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125}})
			}))
			defer server.Close()
			route := harness.ModelRoute{Model: "fixture", BaseURL: server.URL, APIKey: "private-key"}
			generator, e := NewSkillCandidateGenerator(ctx, skillmaintenance.GenerationConfig{Route: route})
			if e != nil {
				t.Fatal(e)
			}
			input := generationInput()
			if _, e = generator.Generate(context.Background(), input); e == nil || calls.Load() != 0 {
				t.Fatalf("unbudgeted generation: %v %d", e, calls.Load())
			}
			candidate, e := generator.Generate(ctx, input)
			if e != nil || len(candidate.Files) != 1 || !strings.Contains(string(candidate.Files["SKILL.md"]), "Verify source claims") {
				t.Fatalf("candidate %+v %v", candidate, e)
			}
			if strings.Contains(string(input.BaseFiles["SKILL.md"]), "Verify source claims") {
				t.Fatal("generation modified the base")
			}
			data, _ := json.Marshal(candidate.Files)
			p := harness.SkillProposal{Action: "edit", SkillName: "research", Reason: candidate.Reason, Base: input.Base, CandidateVersion: fmt.Sprintf("%x", sha256.Sum256(data))}
			validator, e := NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route, Evaluation: route, MaxOutputTokens: 512})
			if e != nil {
				t.Fatal(e)
			}
			report, e := validator.Validate(ctx, harness.SkillValidationInput{Proposal: p, BaseFiles: input.BaseFiles, CandidateFiles: candidate.Files})
			if allowed == 3 && (e != nil || report.Security != "passed" || report.Behavior != "not_run" || len(report.ModelChecks) != 2) {
				t.Fatalf("review %+v %v", report, e)
			}
			if allowed == 2 && e == nil {
				t.Fatal("quality review bypassed shared quota")
			}
			if int64(calls.Load()) != allowed {
				t.Fatalf("calls %d allowed %d", calls.Load(), allowed)
			}
			receipts, e := s.Receipts(context.Background(), c.Job.ID, 0, 100)
			if e != nil || len(receipts) != int(allowed) {
				t.Fatalf("receipts %+v %v", receipts, e)
			}
			for i, stage := range []string{"generation", "security", "quality"} {
				if i < len(receipts) && (receipts[i].Stage != stage || receipts[i].ChargedTokens != 125 || receipts[i].Estimated) {
					t.Fatalf("receipt %+v", receipts[i])
				}
			}
		})
	}
}

func TestSkillCandidateResponseRejectsAmbiguity(t *testing.T) {
	valid := `{"skill_name":"research","base_version":"base","reason":"Clarify verification","files":{"SKILL.md":"text","notes.md":"unchanged"}}`
	if c, e := parseSkillCandidate(valid, "research", "base"); e != nil || len(c.Files) != 2 {
		t.Fatalf("valid %+v %v", c, e)
	}
	for _, bad := range []string{
		strings.Replace(valid, `"skill_name":"research"`, `"skill_name":"research","skill_name":"different"`, 1),
		strings.Replace(valid, `"notes.md":"unchanged"`, `"SKILL.md":"conflicting text"`, 1),
		strings.Replace(valid, `"base_version":"base"`, `"base_version":"changed"`, 1),
		strings.Replace(valid, `"files":`, `"unexpected":`, 1),
		strings.Replace(valid, `"reason":"Clarify verification"`, `"reason":null`, 1),
		strings.Replace(valid, `"SKILL.md":"text"`, `"SKILL.md":null`, 1),
		strings.Replace(valid, `"SKILL.md":"text"`, `"SKILL.md":{"text":"hidden"}`, 1),
		strings.Replace(valid, `"SKILL.md":"text"`, `"other.md":"text"`, 1),
		valid + ` {}`,
		"```json\n" + valid + "\n```",
	} {
		if _, e := parseSkillCandidate(bad, "research", "base"); e == nil {
			t.Fatalf("accepted ambiguous candidate %s", bad)
		}
	}
}
