package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSkillLearningCandidateProvenanceAndRedaction(t *testing.T) {
	input := harness.SkillLearningInput{EpisodeID: "episode", References: []harness.SkillLearningReference{
		{ID: "user", Kind: "user"}, {ID: "receipt", Kind: "tool_observation", ToolName: "read_file", State: "completed"}, {ID: "remote", Kind: "tool_observation", ToolName: "web_fetch", State: "completed"}, {ID: "truncated", Kind: "tool_observation", ToolName: "read_file", State: "completed", Truncated: true},
	}}
	base := harness.SkillExperienceCandidate{Kind: "proven_recipe", Goal: "Inspect JSON", Applicability: "PowerShell", Resolution: "Use full file token=SHOULD_NOT_PERSIST", Steps: []string{"Read complete file"}, Verification: "Receipt recorded", Outcome: "observed_execution", References: []string{"user", "receipt"}}
	for _, mode := range []string{"valid", "foreign", "remote", "truncated", "without-user", "trigger", "preference", "reported-recipe"} {
		c := base
		switch mode {
		case "foreign":
			c.References = []string{"user", "missing"}
		case "remote":
			c.References = []string{"user", "remote"}
		case "truncated":
			c.References = []string{"user", "truncated"}
		case "without-user":
			c.References = []string{"receipt"}
		case "trigger":
			c.Kind = "trigger_mismatch"
		case "preference":
			c.Kind = "preference"
		case "reported-recipe":
			c.Outcome = "user_reported"
		}
		result, err := validateCandidates(input, []harness.SkillExperienceCandidate{c})
		if (err == nil) != (mode == "valid") {
			t.Fatalf("%s accepted invalid source: %v", mode, err)
		}
		if mode == "valid" {
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), "SHOULD_NOT_PERSIST") {
				t.Fatal("credential retained in experience")
			}
		}
	}
}

func TestSkillLearningRedactionRemovesSecretsAndMachinePaths(t *testing.T) {
	for _, text := range []string{
		`{"api_key":"PRIVATE_API_KEY"}`, `token=PRIVATE_TOKEN`, "Authorization: Bearer PRIVATE_AUTH\n next line",
		`https://private.invalid/?token=PRIVATE_QUERY`, `person@private.invalid`, `13800138000`,
		`C:\Users\private\config.json`, `C:/Users/private/config.json`, `\\private-host\private-share\config.json`,
		`/home/private/config.json`, "-----BEGIN PRIVATE KEY-----\nPRIVATE_DATA\n-----END PRIVATE KEY-----",
	} {
		if result := redact(text, 1024); result == text || strings.Contains(strings.ToLower(result), "private") || strings.Contains(result, "13800138000") {
			t.Fatal("sensitive text retained", result)
		}
	}
}

func TestSkillLearningCandidateHiddenCharactersCannotPersist(t *testing.T) {
	input := harness.SkillLearningInput{EpisodeID: "episode", References: []harness.SkillLearningReference{
		{ID: "user", Kind: "user"}, {ID: "receipt", Kind: "tool_observation", ToolName: "read_file", State: "completed"},
	}}
	candidate := harness.SkillExperienceCandidate{Kind: "proven_recipe", Goal: "Inspect complete input; to\u200bken=HIDDEN_PRIVATE_GOAL",
		Applicability: "Local configuration; api_\x01key=HIDDEN_PRIVATE_APPLICABILITY", Problem: "Incomplete input; Bea\u2060rer HIDDEN_PRIVATE_PROBLEM",
		Resolution: "Read whole file; sk\u200b-HIDDEN_PRIVATE_RESOLUTION", Steps: []string{"Read whole file; to\u200bken=HIDDEN_PRIVATE_STEP"},
		Verification: "Complete local receipt; to\u200bken=HIDDEN_PRIVATE_VERIFICATION", Outcome: "observed_execution", References: []string{"user", "receipt"}}
	clean, err := validateCandidates(input, []harness.SkillExperienceCandidate{candidate})
	if err != nil || len(clean) != 1 || clean[0].Outcome != "observed_execution" {
		t.Fatal("redaction lost supported procedural evidence", clean, err)
	}
	encoded, err := json.Marshal(clean)
	if err != nil || strings.Contains(string(encoded), "HIDDEN_PRIVATE") {
		t.Fatal("candidate output retained an obfuscated secret", err)
	}
	for _, text := range []string{clean[0].Goal, clean[0].Applicability, clean[0].Problem, clean[0].Resolution, clean[0].Steps[0], clean[0].Verification} {
		if !strings.Contains(text, "[redacted]") || strings.HasPrefix(text, "[redacted]") {
			t.Fatal("candidate redaction erased the procedure or missed a field")
		}
	}
	if replay, err := validateCandidates(input, clean); err != nil || encodedDigest(replay) != encodedDigest(clean) {
		t.Fatal("sanitized candidate failed saved-result validation", replay, err)
	}
}

func TestSkillLearningCallSublimitLeavesSharedBudgetForReview(t *testing.T) {
	db, err := sqlite.Open(t.TempDir() + "/quota.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{options: harness.SkillLearningOptions{MaxDailyModelCalls: 1, MaxDailyTokens: 1000}}
	queue, err := skillmaintenance.NewWithHooks(context.Background(), db.DB(), skillmaintenance.Policy{MaxDailyModelCalls: 3, MaxDailyTokens: 10000, MaxJobSeconds: 60, Workers: 1}, skillmaintenance.Hooks{CallAdmission: func(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job, call skillmaintenance.ModelCall, now time.Time) error {
		if job.Request.Kind == skillmaintenance.ExperienceExtract {
			return s.AdmitCallTx(ctx, tx, job, call, now)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		j, err := queue.Enqueue(context.Background(), skillmaintenance.Request{Kind: skillmaintenance.ExperienceExtract, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: string(rune('a' + i)), Reference: "snapshot", InputDigest: strings.Repeat("b", 64)})
		if err != nil {
			t.Fatal(err)
		}
		claim, ok, err := queue.ClaimKinds(context.Background(), []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract})
		if err != nil || !ok || claim.Job.ID != j.ID {
			t.Fatal(err, ok)
		}
		b := queue.Broker(claim)
		r, err := b.Reserve(context.Background(), skillmaintenance.ModelCall{Stage: "experience_extract", ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 100})
		if i == 0 {
			if err != nil {
				t.Fatal(err)
			}
			if err = b.Settle(context.Background(), r, skillmaintenance.Usage{}); err != nil {
				t.Fatal(err)
			}
		} else if err != skillmaintenance.ErrBudget {
			t.Fatalf("sublimit bypassed %v", err)
		}
		code := ""
		if i == 1 {
			code = "budget_exhausted"
		}
		if err = queue.Finish(context.Background(), claim, code, false); err != nil {
			t.Fatal(err)
		}
	}
	_, err = queue.Enqueue(context.Background(), skillmaintenance.Request{SourceIdentity: strings.Repeat("a", 64), Name: "sample", Key: "review", Reference: "proposal", InputDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := queue.Claim(context.Background())
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if _, err = queue.Broker(claim).Reserve(context.Background(), skillmaintenance.ModelCall{Stage: "security", ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 100}); err != nil {
		t.Fatal("learning starved review", err)
	}
}
