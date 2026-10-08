package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestApplyConfigSkillReviewUsesConfiguredRoutesAndCustomSource(t *testing.T) {
	var calls [2]atomic.Int32
	server := func(index int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[index].Add(1)
			var req struct {
				Model    string                     `json:"model"`
				Messages []struct{ Content string } `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				http.Error(w, "bad", 400)
				return
			}
			if req.Model != "shared-model-id" || len(req.Messages) != 2 {
				t.Errorf("model route: %+v", req)
			}
			var input map[string]any
			if err := json.Unmarshal([]byte(req.Messages[1].Content), &input); err != nil {
				t.Error(err)
			}
			content, _ := json.Marshal(map[string]any{"base_version": input["base_version"], "candidate_version": input["candidate_version"], "passed": true, "reason": "Independent configured review"})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(content)}, "finish_reason": "stop"}}})
		}))
	}
	moderator, evaluator := server(0), server(1)
	defer moderator.Close()
	defer evaluator.Close()
	skills := t.TempDir()
	if err := os.Mkdir(filepath.Join(skills, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(skills, "public"), 0700); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(strings.ReplaceAll(roleModels, "https://one.invalid/v1", moderator.URL+"/v1"), "https://two.invalid/v1", evaluator.URL+"/v1")
	body += fmt.Sprintf("skills:\n  enabled: true\n  path: %s\nskill_evolution:\n  enabled: true\n  mode: review\n  moderation_model_name: main\n  evaluation_model_name: research\n  security_fail_closed: true\n  validation:\n    max_output_tokens: 512\n", filepath.ToSlash(skills))
	path := writeRoleConfig(t, body)
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillValidator == nil || len(cfg.SkillProposalSources) != 1 || cfg.SkillProposalSources[0] != "python-custom" {
		t.Fatalf("model tool sources: %+v", cfg.SkillProposalSources)
	}
	if len(cfg.Skills.BuiltinSources) != 1 || cfg.Skills.BuiltinSources[0] != "python-public" {
		t.Fatalf("missing builtin source policy: %+v", cfg.Skills)
	}
	report, err := cfg.SkillValidator.Validate(context.Background(), harness.SkillValidationInput{Proposal: harness.SkillProposal{Action: "create", SkillName: "research", CandidateVersion: "candidate"}, CandidateFiles: map[string][]byte{"SKILL.md": []byte("Fixture content")}})
	if err != nil || report.Security != "passed" || calls[0].Load() != 1 || calls[1].Load() != 1 {
		t.Fatalf("resolved review calls: %+v %v %d/%d", report, err, calls[0].Load(), calls[1].Load())
	}
	withOverlays := strings.Replace(body, "  mode: review", "  mode: review\n  allow_builtin_overlays: true", 1)
	if err = os.WriteFile(path, []byte(withOverlays), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyConfig(path, &cfg, &connections, nil); err != nil || len(cfg.SkillProposalSources) != 2 || cfg.SkillProposalSources[1] != "python-public" {
		t.Fatalf("explicit builtin overlay grant: %+v %v", cfg.SkillProposalSources, err)
	}
	if err = os.WriteFile(path, []byte(strings.Replace(body, "  enabled: true\n  mode: review", "  enabled: false\n  mode: review", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyConfig(path, &cfg, &connections, nil); err != nil || len(cfg.SkillProposalSources) != 0 {
		t.Fatalf("explicit disable preserved model writes: %+v %v", cfg.SkillProposalSources, err)
	}
}

func TestApplyConfigSkillReviewRejectsInactiveSettingsAndMissingDestinations(t *testing.T) {
	for _, section := range []string{
		"enabled: true\n  moderation_model_name: main\n  evaluation_model_name: research\n",
		"enabled: true\n  moderation_model_name: missing\n  evaluation_model_name: research\n",
		"enabled: true\n  mode: auto_patch\n  moderation_model_name: main\n  evaluation_model_name: research\n",
		"enabled: true\n  moderation_model_name: main\n  evaluation_model_name: research\n  security_fail_closed: false\n",
	} {
		path := writeRoleConfig(t, roleModels+"skill_evolution:\n  "+section)
		var cfg deerflow.Config
		connections := 0
		if _, err := ApplyConfig(path, &cfg, &connections, nil); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
			t.Fatalf("ignored setting %q: %v", section, err)
		}
	}
}

func TestApplyConfigSkillMaintenanceInheritsFirstModelWithoutGlobalDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(roleModels, "default_model: main\n", "", 1)
	body = strings.Replace(body, "local_acp:\n", "local_acp:\n  model_name: research\n", 1)
	body += fmt.Sprintf("skills:\n  path: %s\nskill_evolution:\n  enabled: true\n  generation:\n    enabled: true\n  budget:\n    max_daily_model_calls: 3\n    max_daily_tokens: 100000\n    max_job_seconds: 30\n    workers: 1\n", filepath.ToSlash(root))
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(writeRoleConfig(t, body), &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://two.invalid/v1" || cfg.SkillGeneration == nil || cfg.SkillGeneration.Model.Route.BaseURL != "https://one.invalid/v1" || cfg.SkillGeneration.Model.Route.APIKey != "fixture-one" {
		t.Fatal("maintenance did not inherit the first model independently of the session override")
	}
}
