package desktopconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"go.yaml.in/yaml/v3"
)

func TestEvolutionLegacyConversionIsExplicitAndRevisionBound(t *testing.T) {
	options, _ := reviewDocument(t)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"enabled": false, "mode": "auto_patch", "security_fail_closed": false,
		"moderation_model_name": "first", "evaluation_model_name": "second",
		"discovery":             map[string]any{"token": "fixture-discovery-secret"},
		"generation_model_name": "first",
		"validation":            map[string]any{"max_input_bytes": 4096, "old_options": "fixture-validation-secret"},
	}
	raw["skill_evolution"] = legacy
	raw["outside_evolution"] = map[string]any{"token": "fixture-outside-secret"}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	encoded := mustJSON(t, doc)
	for _, secret := range []string{"fixture-discovery-secret", "fixture-validation-secret", "fixture-outside-secret"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("snapshot exposed inactive data")
		}
	}
	migration := readObject(doc, "skill_evolution_migration")
	wantPaths := []string{"skill_evolution.discovery.token", "skill_evolution.validation.old_options"}
	if migration["required"] != true || !reflect.DeepEqual(migration["removed_paths"], wantPaths) {
		t.Fatalf("migration paths: %+v", migration)
	}
	if readObject(doc, "skill_evolution")["generation_model_name"] != "first" || readObject(readObject(doc, "skill_evolution"), "generation")["enabled"] != false {
		t.Fatal("legacy model reference activated generation or was discarded")
	}
	// Unrelated edits retain dormant data, even a previously accepted mode.
	readObject(doc, "runtime")["max_active_runs"] = 3
	doc = run(t, options, "save", doc)
	saved, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	got := readObject(saved, "skill_evolution")
	if got["mode"] != "auto_patch" || got["security_fail_closed"] != false || readObject(got, "discovery")["token"] != "fixture-discovery-secret" || readObject(got, "discovery")["operator_corrections_enabled"] != false || readObject(got, "validation")["old_options"] != "fixture-validation-secret" {
		t.Fatal("unrelated save changed legacy data")
	}
	before, _ := os.ReadFile(options.Config)
	for _, ack := range []any{"stale-revision", true} {
		doc["skill_evolution_migration_ack"] = ack
		if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
			t.Fatal("invalid migration acknowledgement accepted")
		}
		after, _ := os.ReadFile(options.Config)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid migration wrote config")
		}
	}
	doc["skill_evolution_migration_ack"] = ""
	review := readObject(doc, "skill_evolution")
	review["mode"], review["security_fail_closed"], review["enabled"] = "review", true, true
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
		t.Fatal("enabled hidden legacy settings without conversion", err)
	}
	review["enabled"] = false
	doc["skill_evolution_migration_ack"] = doc["config_revision"]
	doc = run(t, options, "save", doc)
	backup, err := os.ReadFile(filepath.Join(doc["backup"].(string), filepath.Base(options.Config)))
	if err != nil || !bytes.Equal(before, backup) {
		t.Fatal("conversion did not back up original bytes", err)
	}
	saved, err = readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	got = readObject(saved, "skill_evolution")
	_, ignored, err := configschema.ProjectEvolution(got)
	if err != nil || len(ignored) != 0 || got["enabled"] != false || got["mode"] != "review" || got["security_fail_closed"] != true {
		t.Fatal("conversion policy or cleanup incorrect", err)
	}
	if got["moderation_model_name"] != "first" || got["evaluation_model_name"] != "second" || readObject(got, "validation")["max_input_bytes"] != 4096 || !reflect.DeepEqual(saved["outside_evolution"], raw["outside_evolution"]) {
		t.Fatal("conversion lost supported or unrelated values")
	}
	if got["generation_model_name"] != "first" || readObject(got, "generation")["enabled"] != false {
		t.Fatal("conversion changed generation opt-in or model selection")
	}
	if readObject(doc, "skill_evolution_migration")["required"] != false || doc["skill_evolution_migration_ack"] != "" {
		t.Fatal("conversion intent not cleared")
	}
	for _, name := range []string{"first", "second"} {
		if modelByName(t, saved, name)["api_key"] != name+"-private-key" {
			t.Fatal("conversion lost credentials")
		}
	}
	// The regular revision guard also binds conversion to bytes actually read.
	doc["skill_evolution_migration_ack"] = doc["config_revision"]
	saved["external_edit"] = true
	if err := writeYAMLAtomic(options.Config, saved); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("stale source revision accepted")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("stale conversion overwrote external edit")
	}
}

func TestEvolutionNewInvalidEditsRejectedEvenWhenDisabled(t *testing.T) {
	for _, edit := range []struct {
		key   string
		value any
	}{
		{"max_input_bytes", 1023}, {"max_input_bytes", 2097153}, {"max_output_tokens", 127},
		{"max_output_tokens", 8193}, {"timeout_seconds", .5}, {"timeout_seconds", 301},
		{"security_fail_closed", false}, {"mode", "unsupported"}, {"moderation_model_name", "unknown"},
	} {
		t.Run(fmt.Sprintf("%s-%v", edit.key, edit.value), func(t *testing.T) {
			options, doc := reviewDocument(t)
			review := readObject(doc, "skill_evolution")
			review["enabled"] = false
			target := review
			if strings.HasPrefix(edit.key, "max_") || edit.key == "timeout_seconds" {
				target = readObject(review, "validation")
			}
			target[edit.key] = edit.value
			before, _ := os.ReadFile(options.Config)
			for _, op := range []string{"validate", "save"} {
				if _, err := Run(options, op, mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
					t.Fatalf("%s: %v", op, err)
				}
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid edit changed file")
			}
		})
	}
}

func TestEvolutionDormantLimitsPreservedButCannotEnable(t *testing.T) {
	options, _ := reviewDocument(t)
	raw, _ := readYAML(options.Config)
	readObject(raw, "skill_evolution")["validation"] = map[string]any{"max_input_bytes": 1}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "save", run(t, options, "snapshot", nil))
	if readObject(readObject(doc, "skill_evolution"), "validation")["max_input_bytes"] != 1 {
		t.Fatal("dormant limit changed")
	}
	readObject(doc, "skill_evolution")["enabled"] = true
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "max_input_bytes") {
		t.Fatal("enabled invalid limit", err)
	}
}

func TestEvolutionSnapshotRejectsMalformedObjects(t *testing.T) {
	for _, value := range []any{"malformed", []any{}, map[string]any{"enabled": "yes"}, map[string]any{"validation": "bad"}} {
		options := setup(t)
		run(t, options, "init", nil)
		raw, _ := readYAML(options.Config)
		raw["skill_evolution"] = value
		if err := writeYAMLAtomic(options.Config, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(options, "snapshot", nil); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
			t.Fatal("malformed section hidden", err)
		}
	}
}

func TestEvolutionTemplatesHaveNativeProfileDefaults(t *testing.T) {
	for _, path := range []string{"config.example.yaml", "resources/default-config.yaml", "resources/desktop-default-config.yaml", "desktop-app/resources/deerflow/default-config.yaml"} {
		t.Run(path, func(t *testing.T) {
			raw, err := readYAML(filepath.Join("..", "..", "..", filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			review := readObject(raw, "skill_evolution")
			projection, ignored, err := configschema.ProjectEvolution(review)
			if err != nil || len(ignored) != 0 || projection["enabled"] != isDesktopTemplate(path) {
				t.Fatal("template contains unsupported settings", ignored, err)
			}
			if err := configschema.ValidateEvolutionDraft(projection, nil); err != nil {
				t.Fatal(err)
			}
			desktop := isDesktopTemplate(path)
			wantMode := "review"
			if desktop {
				wantMode = "auto_patch"
			}
			if projection["mode"] != wantMode || readObject(projection, "auto_patch")["experience_patterns_enabled"] != desktop || readObject(review, "auto_patch")["experience_patterns_enabled"] != desktop {
				t.Fatal("template automatic maintenance differs from its desktop/ACP policy")
			}
			if projection["security_fail_closed"] != true || readObject(projection, "discovery")["experience_new_skills_enabled"] != false || readObject(projection, "learning")["cross_session_enabled"] != false {
				t.Fatal("template widened publication or sharing authority")
			}
			review["enabled"] = true
			encoded, err := yaml.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			settings, err := evolution.DecodeReviewConfig(encoded, map[string]bool{"first": true, "second": true}, "first")
			wantGeneration, wantInputBytes := "", 131072
			if isDesktopTemplate(path) {
				wantGeneration, wantInputBytes = "first", 65536
			}
			if err != nil || settings.ModerationModel != "first" || settings.EvaluationModel != "first" || settings.GenerationModel != wantGeneration || settings.Validation.TimeoutSeconds != 60 || settings.Validation.MaxInputBytes != wantInputBytes || settings.Validation.MaxOutputTokens != 1024 {
				t.Fatal("template cannot enable native defaults", err)
			}
		})
	}
}

func TestEvolutionSaveLaunchUsesReviewRoutesAndBounds(t *testing.T) {
	options, doc := reviewDocument(t)
	var calls [2]atomic.Int32
	var stall atomic.Bool
	for index, name := range []string{"first", "second"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[index].Add(1)
			var request struct {
				Model     string                     `json:"model"`
				MaxTokens int                        `json:"max_completion_tokens"`
				Messages  []struct{ Content string } `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "bad request", 400)
				return
			}
			if request.Model != "shared-review-model" || request.MaxTokens != 512 || len(request.Messages) != 2 || r.Header.Get("Authorization") != "Bearer "+name+"-private-key" {
				t.Errorf("saved route mismatch: model=%q, max_tokens=%d, messages=%d, credentials_match=%t", request.Model, request.MaxTokens, len(request.Messages), r.Header.Get("Authorization") == "Bearer "+name+"-private-key")
				http.Error(w, "bad routing", 400)
				return
			}
			if stall.Load() {
				select {
				case <-r.Context().Done():
				case <-time.After(6 * time.Second):
					t.Error("review transport did not cancel fixture request")
				}
				return
			}
			var evidence map[string]any
			if err := json.Unmarshal([]byte(request.Messages[1].Content), &evidence); err != nil {
				t.Error(err)
			}
			answer, _ := json.Marshal(map[string]any{"base_version": evidence["base_version"], "candidate_version": evidence["candidate_version"], "passed": true, "reason": "Fixture review"})
			_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}})
		}))
		defer server.Close()
		model := modelByName(t, doc, name)
		model["provider"], model["model"], model["base_url"] = "openai", "shared-review-model", server.URL+"/v1"
		// Changing endpoint intentionally clears hidden credentials. Supply the
		// fixture credential explicitly for this newly selected endpoint.
		model["api_key"] = name + "-private-key"
	}
	readObject(readObject(doc, "skill_evolution"), "validation")["max_input_bytes"] = 4096
	readObject(readObject(doc, "skill_evolution"), "validation")["timeout_seconds"] = 1
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillValidator == nil || !reflect.DeepEqual(cfg.SkillProposalSources, []string{"python-custom"}) {
		t.Fatal("saved review not installed")
	}
	input := harness.SkillValidationInput{Proposal: harness.SkillProposal{Action: "create", SkillName: "example", CandidateVersion: "candidate"}, CandidateFiles: map[string][]byte{"SKILL.md": []byte("Example skill")}}
	report, err := cfg.SkillValidator.Validate(context.Background(), input)
	if err != nil || report.Security != "passed" || report.Behavior != "not_run" || calls[0].Load() != 1 || calls[1].Load() != 1 {
		t.Fatal("saved reviewer routes not used", err)
	}
	input.CandidateFiles["SKILL.md"] = bytes.Repeat([]byte("a"), 4096)
	if _, err := cfg.SkillValidator.Validate(context.Background(), input); err == nil || !strings.Contains(err.Error(), "bounds") || calls[0].Load() != 1 || calls[1].Load() != 1 {
		t.Fatal("saved input bound not enforced before model call", err)
	}
	input.CandidateFiles["SKILL.md"] = []byte("Example skill")
	stall.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cfg.SkillValidator.Validate(ctx, input); err == nil || ctx.Err() != nil || calls[0].Load() != 2 || calls[1].Load() != 1 {
		t.Fatal("saved one-second timeout did not cancel review before outer deadline", err)
	}
}
