package deerflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillManageValidationFeedback(t *testing.T) {
	for _, tc := range []struct {
		name, status, security, behavior, quality, code, stage string
		checks                                                 []harness.SkillModelCheck
	}{
		{"quality-rejection", "failed", "passed", "not_run", "failed", "quality_rejected", "quality", []harness.SkillModelCheck{{Stage: "security", Passed: true}, {Stage: "quality", Reason: "Missing output consistency check."}}},
		{"security-rejection", "failed", "failed", "not_run", "not_run", "security_rejected", "security", []harness.SkillModelCheck{{Stage: "security", Reason: "Overbroad access."}}},
		{"behavior-rejection", "failed", "passed", "failed", "passed", "behavior_failed", "behavior", []harness.SkillModelCheck{{Stage: "quality", Passed: true}}},
		{"legacy-unknown", "failed", "passed", "not_run", "unknown", "validation_incomplete", "validation", nil},
		{"reviewed", "pending_review", "passed", "not_run", "passed", "", "", []harness.SkillModelCheck{{Stage: "security", Passed: true}, {Stage: "quality", Passed: true}}},
		{"legacy-custom-validator", "pending_review", "passed", "not_run", "unknown", "", "", nil},
		{"queued", "validating", "", "", "not_run", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := harness.SkillProposal{ID: "p_fixture", Status: tc.status, Evaluation: harness.SkillValidationReport{Security: tc.security, Behavior: tc.behavior, ModelChecks: tc.checks}}
			before, _ := json.Marshal(p)
			var result map[string]any
			if err := json.Unmarshal([]byte(skillManageResult(p)), &result); err != nil {
				t.Fatal(err)
			}
			if result["quality"] != tc.quality || result["error_code"] != tc.code || result["failure_stage"] != tc.stage || result["retryable"] != false || result["live_skill_changed"] != false {
				t.Fatalf("incomplete or misleading feedback: %+v", result)
			}
			if result["ok"] != true || result["requires_operator_review"] != (tc.status == "pending_review") {
				t.Fatalf("submission contract changed: %+v", result)
			}
			if tc.status == "failed" && (result["error"] == "" || result["next_action"] == "") {
				t.Fatalf("failure lost its explanation: %+v", result)
			}
			if tc.code == "quality_rejected" && !strings.Contains(skillManageResult(p), "Missing output consistency check.") {
				t.Fatal("quality rejection reason was hidden from the proposing agent")
			}
			after, _ := json.Marshal(p)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("feedback changed persisted evidence")
			}
		})
	}
}

func TestSkillManageValidationFeedbackRedactsReasonsWithoutChangingEvidence(t *testing.T) {
	p := harness.SkillProposal{Status: "failed", Evaluation: harness.SkillValidationReport{Security: "passed", Behavior: "not_run", ModelChecks: []harness.SkillModelCheck{
		{Stage: "quality", Reason: "Missing check; api_key=fixture-secret https://private.example/review?token=hidden", Provider: "private-provider", Model: "private-model"},
	}}}
	before, _ := json.Marshal(p)
	result := skillManageResult(p)
	for _, secret := range []string{"fixture-secret", "private.example", "token=hidden"} {
		if strings.Contains(result, secret) {
			t.Fatalf("feedback exposed review credentials: %s", result)
		}
	}
	if !strings.Contains(result, "Missing check") || !strings.Contains(result, "[redacted]") {
		t.Fatalf("safe diagnostic evidence is missing: %s", result)
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("sanitizing tool feedback rewrote immutable review evidence")
	}
}

func TestSkillManageValidationFeedbackKeepsSpecificLegacyFailure(t *testing.T) {
	p := harness.SkillProposal{Status: "failed", Error: "The generated skill contains a credential-like literal.", Evaluation: harness.SkillValidationReport{Security: "passed", Behavior: "not_run"}}
	var result map[string]any
	if err := json.Unmarshal([]byte(skillManageResult(p)), &result); err != nil {
		t.Fatal(err)
	}
	if result["error"] != p.Error || result["retryable"] != false || result["error_code"] != "validation_incomplete" {
		t.Fatalf("projection hid a specific host failure or invented a transport cause: %+v", result)
	}
}
