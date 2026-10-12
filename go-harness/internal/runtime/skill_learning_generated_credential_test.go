package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func TestSkillLearningGeneratedCredentialsRejectedBeforeStaging(t *testing.T) {
	for _, test := range []struct {
		name, instruction string
		newSkill          bool
		automatic         bool
	}{
		{"unquoted-assignment", "Use token=GENERATED_PRIVATE_ASSIGNMENT\n", false, false},
		{"bearer", "Authorization: Bearer GENERATED_PRIVATE_BEARER\n", false, false},
		{"json-field", "Example: {\"access_token\":\"GENERATED_PRIVATE_JSON\"}\n", false, false},
		{"escaped-key", "Example: {\"to\\u006ben\":\"GENERATED_PRIVATE_ESCAPED\"}\n", false, false},
		{"hidden-key", "Use to\u200bken=GENERATED_PRIVATE_HIDDEN\n", false, false},
		{"provider-prefix", "Use sk\u2060-GENERATED_PRIVATE_PROVIDER\n", false, false},
		{"partial-private-key", "-----BEGIN PRIVATE KEY-----\nGENERATED_PRIVATE_PEM\n", false, false},
		{"new-capability", "Use token=GENERATED_PRIVATE_NEW\n", true, false},
		{"automatic-existing", "Use token=GENERATED_PRIVATE_AUTOMATIC\n", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, x, r, e, g, v, matcher := experienceDiscoveryTest(t)
			ctx := context.Background()
			if test.newSkill {
				matcher.relation, matcher.suggestedName = "new_capability", "complete-json-audit"
			}
			if test.automatic {
				adoptLearningBase(t, r)
				g.preserveFrontmatter = true
				if err := e.ConfigureBehavior(&autoPatchLearningEvaluator{}, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
					t.Fatal(err)
				}
				experienceTwoEpisodes(t, s, x)
				enableExperienceDiscovery(t, e)
				if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
					t.Fatal(err)
				}
			} else {
				consolidateExtract(t, s, x, "Inspect complete local configuration")
				consolidateRun(t, s)
				consolidateExtract(t, s, x, "Correct the check using the complete input")
				consolidateRun(t, s)
				experienceMatch(t, s)
				enableExperienceReviewDrafts(t, e, test.newSkill)
			}
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 || ds[0].ReviewDraft == test.automatic {
				t.Fatal("automatic single-task draft was not admitted", ds)
			}
			g.extraInstruction = test.instruction
			if worked, err := e.RunGeneration(ctx); err != nil || !worked {
				t.Fatal("generation did not run", worked, err)
			}
			job, err := e.GetGeneration(ctx, ds[0].GenerationID)
			if err != nil || job.State != "failed" || job.ErrorCode != "invalid_candidate" || job.CandidateVersion != "" || job.ProposalID != "" || job.Attempts != 1 {
				t.Fatal("generated credential reached retained candidate or review", job, err)
			}
			if len(g.seen) != 1 || !g.seen[0].InstructionOnly || v.calls != 0 {
				t.Fatal("unsafe output reached paid content review", len(g.seen), v.calls)
			}
			versions, proposals, episodes := 1, 0, 1
			if test.automatic {
				versions, proposals, episodes = 2, 1, 2
			}
			for _, check := range []struct {
				query string
				want  int
			}{
				{`SELECT count(*) FROM df_skill_versions`, versions},
				{`SELECT count(*) FROM df_learning_owned_versions`, 0},
				{`SELECT count(*) FROM df_skill_proposals`, proposals},
				{`SELECT count(*) FROM df_skill_discovery_episode_consumed`, episodes},
				{`SELECT COALESCE(sum(reservations),0) FROM df_skill_discovery_days`, 1},
			} {
				var count int
				if err := s.Store.db.QueryRow(check.query).Scan(&count); err != nil || count != check.want {
					t.Fatal("unsafe output retained data or refunded admission", check.query, count, err)
				}
			}
			receipts, err := e.GenerationReceipts(ctx, job.ID, 0, 10)
			if err != nil || len(receipts) != 1 || receipts[0].Stage != "generation" || receipts[0].ChargedTokens != 15 {
				t.Fatal("failed generation spending was lost or expanded", receipts, err)
			}
			if err := e.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.WithPublication(ctx, ds[0].Target.SourceID, ds[0].Target.Name, func(tx *skills.PublicationTx) error {
				head, err := tx.Current()
				if err == nil && head != ds[0].Base {
					t.Fatal("unsafe candidate changed installed head", head)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if worked, err := e.RunGeneration(ctx); err != nil || worked || len(experienceDiscoveries(t, e)) != 1 || len(g.seen) != 1 {
				t.Fatal("credential rejection caused automatic regeneration", worked, err)
			}
			if test.newSkill {
				var reservations int
				if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_new_skill_consumed`).Scan(&reservations); err != nil || reservations != 1 {
					t.Fatal("invalid output refunded lifetime creation reservation", reservations, err)
				}
			}
		})
	}
}

func TestSkillLearningGeneratedLegacyCredentialsCannotResumeOrPublish(t *testing.T) {
	for _, phase := range []string{"candidate", "validating-proposal", "pending-review", "terminal-review-ack"} {
		t.Run(phase, func(t *testing.T) {
			s, x, r, e, g, v, _ := experienceDiscoveryTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "Inspect complete local configuration")
			consolidateRun(t, s)
			consolidateExtract(t, s, x, "Correct the check using complete input")
			consolidateRun(t, s)
			experienceMatch(t, s)
			enableExperienceReviewDrafts(t, e, false)
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			if phase == "candidate" {
				if _, err := s.Store.db.Exec(`CREATE TRIGGER legacy_credential_proposal_fault BEFORE INSERT ON df_skill_proposals BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			} else if phase == "validating-proposal" {
				v.failures = 1
			} else if phase == "terminal-review-ack" {
				if _, err := s.Store.db.Exec(`CREATE TRIGGER legacy_credential_finish_fault BEFORE UPDATE ON df_skill_maintenance_jobs WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if worked, err := e.RunGeneration(ctx); !worked || (err != nil) != (phase == "terminal-review-ack") {
				t.Fatal(worked, err)
			}
			job, err := e.GetGeneration(ctx, ds[0].GenerationID)
			if err != nil || job.CandidateVersion == "" || len(g.seen) != 1 || (job.ProposalID == "") != (phase == "candidate") {
				t.Fatal("healthy checkpoint not saved", job, err)
			}
			if (phase == "pending-review") != (job.State == "completed") {
				t.Fatal("wrong checkpoint state", job)
			}
			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.db.Exec(`DROP TRIGGER IF EXISTS legacy_credential_proposal_fault; DROP TRIGGER IF EXISTS legacy_credential_finish_fault`); err != nil {
				t.Fatal(err)
			}
			job = retainLegacyCredentialCandidate(t, r, e, job)
			beforeCalls := v.calls
			var beforeTokens int
			if err := s.Store.db.QueryRow(`SELECT COALESCE(sum(charged_tokens),0) FROM df_skill_maintenance_calls`).Scan(&beforeTokens); err != nil {
				t.Fatal(err)
			}
			next, err := evolution.New(ctx, r, v)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = next.Close() })
			if err := next.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			if err := next.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
				t.Fatal(err)
			}
			if phase != "pending-review" {
				if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0,lease_ns=0 WHERE id=?`, job.ID); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "pending-review" {
				p, err := next.Get(ctx, job.ProposalID)
				if err != nil || p.Status != "pending_review" {
					t.Fatal("retained old proposal not valid", p, err)
				}
				p, err = next.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
				if !errors.Is(err, harness.ErrSkillValidation) || p.Status != "failed" || strings.Contains(p.Error, "GENERATED_PRIVATE") {
					t.Fatal("human review bypassed current credential guard", p, err)
				}
			} else {
				enableExperienceReviewDrafts(t, next, false)
				if worked, err := next.RunGeneration(ctx); err != nil || !worked {
					t.Fatal("recovery did not run", worked, err)
				}
				failed, err := next.GetGeneration(ctx, job.ID)
				if err != nil || failed.State != "failed" || failed.ErrorCode != "invalid_candidate" || failed.CandidateVersion != job.CandidateVersion {
					t.Fatal("unsafe saved checkpoint resumed", failed, err)
				}
				if phase == "validating-proposal" {
					p, err := next.Get(ctx, job.ProposalID)
					if err != nil || p.Status != "failed" {
						t.Fatal("unsafe validating proposal remained active", p, err)
					}
				} else if phase == "terminal-review-ack" {
					p, err := next.Get(ctx, job.ProposalID)
					if err != nil || p.Status != "pending_review" {
						t.Fatal("terminal review fixture was not retained", p, err)
					}
					p, err = next.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
					if !errors.Is(err, harness.ErrSkillValidation) || p.Status != "failed" {
						t.Fatal("unsafe terminal review remained publishable", p, err)
					}
				}
			}
			if len(g.seen) != 1 || v.calls != beforeCalls {
				t.Fatal("legacy rejection regenerated or re-reviewed", len(g.seen), v.calls)
			}
			var afterTokens, consumed int
			if err := s.Store.db.QueryRow(`SELECT COALESCE(sum(charged_tokens),0) FROM df_skill_maintenance_calls`).Scan(&afterTokens); err != nil || afterTokens != beforeTokens {
				t.Fatal("legacy rejection changed spending", afterTokens, beforeTokens, err)
			}
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumed); err != nil || consumed != 1 {
				t.Fatal("legacy rejection refunded evidence", consumed, err)
			}
			if err := r.WithPublication(ctx, job.Target.SourceID, job.Target.Name, func(tx *skills.PublicationTx) error {
				head, err := tx.Current()
				if err == nil && head != job.Base {
					t.Fatal("legacy candidate changed installed head", head)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Retain a correctly hashed snapshot of output an older host could accept.
// This uses real Registry validation/digests, not corrupt bytes or fake hashes.
func retainLegacyCredentialCandidate(t *testing.T, r *skills.Registry, e *evolution.Service, job harness.SkillGenerationJob) harness.SkillGenerationJob {
	t.Helper()
	ctx := context.Background()
	var files, baseFiles map[string][]byte
	if err := r.WithPublication(ctx, job.Target.SourceID, job.Target.Name, func(tx *skills.PublicationTx) error {
		candidate, err := tx.ReadVersion(job.CandidateVersion)
		if err != nil {
			return err
		}
		files = candidate.Files()
		if job.Base.Exists {
			base, err := tx.ReadVersion(job.Base.Version)
			if err != nil {
				return err
			}
			baseFiles = base.Files()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	files["SKILL.md"] = append(files["SKILL.md"], []byte("Use to\u200bken=GENERATED_PRIVATE_LEGACY\n")...)
	candidate, err := r.PrepareCandidate(job.Target.Name, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range candidate.Findings() {
		if finding.Code == "possible_credential" {
			t.Fatal("fixture no longer represents the old scan gap")
		}
	}
	var p harness.SkillProposal
	if job.ProposalID != "" {
		p, err = e.Get(ctx, job.ProposalID)
		if err != nil {
			t.Fatal(err)
		}
		p.CandidateVersion = candidate.Version()
		p.Evaluation.CandidateVersion = candidate.Version()
		p.EvaluationDigest = generatedCredentialDigest(t, p.Evaluation)
		lines := func(data []byte) []string {
			return strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
		}
		old, updated := lines(baseFiles["SKILL.md"]), lines(files["SKILL.md"])
		p.Diff = "--- a/SKILL.md\n+++ b/SKILL.md\n@@ -1," + fmt.Sprint(len(old)) + " +1," + fmt.Sprint(len(updated)) + " @@\n"
		for _, line := range old {
			p.Diff += "-" + line + "\n"
		}
		for _, line := range updated {
			p.Diff += "+" + line + "\n"
		}
	}
	if err := r.WithPublication(ctx, job.Target.SourceID, job.Target.Name, func(tx *skills.PublicationTx) error {
		version, err := tx.Stage(candidate)
		if err != nil {
			return err
		}
		if _, err := tx.SQL().ExecContext(ctx, `INSERT OR IGNORE INTO df_learning_owned_versions(source_key,name,version) VALUES(?,?,?)`, job.Target.SourceIdentity, job.Target.Name, version); err != nil {
			return err
		}
		var reason string
		if err := tx.SQL().QueryRowContext(ctx, `SELECT candidate_reason FROM df_skill_generation_inputs WHERE job_id=?`, job.ID).Scan(&reason); err != nil {
			return err
		}
		progress := generatedCredentialDigest(t, []string{version, reason, job.ProposalID})
		if _, err := tx.SQL().ExecContext(ctx, `UPDATE df_skill_generation_inputs SET candidate_version=?,progress_sha=? WHERE job_id=?`, version, progress, job.ID); err != nil {
			return err
		}
		if p.ID != "" {
			var key string
			if err := tx.SQL().QueryRowContext(ctx, `SELECT request_key FROM df_skill_proposals WHERE id=?`, p.ID).Scan(&key); err != nil {
				return err
			}
			request := struct {
				Schema                                                     int
				SourceID, Name, RequestKey, Action, Reason, Author, Origin string
				Base                                                       harness.SkillHead
				Trigger                                                    harness.SkillTrigger
				Version                                                    string
			}{1, p.Target.SourceID, p.SkillName, key, p.Action, p.Reason, p.Author, p.Origin, p.Base, p.Trigger, version}
			data, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if _, err := tx.SQL().ExecContext(ctx, `UPDATE df_skill_proposals SET request_sha=?,payload=? WHERE id=?`, generatedCredentialDigest(t, request), data, p.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	job.CandidateVersion = candidate.Version()
	return job
}

func generatedCredentialDigest(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestSkillLearningGeneratedGenericInstructionsKeepExactBytes(t *testing.T) {
	s, x, r, e, g, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Inspect complete local configuration")
	consolidateRun(t, s)
	consolidateExtract(t, s, x, "Correct the check using the complete input")
	consolidateRun(t, s)
	experienceMatch(t, s)
	enableExperienceReviewDrafts(t, e, false)
	if ds := experienceDiscoveries(t, e); len(ds) != 1 {
		t.Fatal("generic draft not admitted", ds)
	}
	g.extraInstruction = "Read https://docs.example.invalid/guide; use /tmp/scratch for temporary output.\n" +
		"Use token=${SERVICE_TOKEN}, api_key=\"$API_KEY\", and password=%PASSWORD%.\n" +
		"Examples use {\"token\":\"<TOKEN>\"}, token=[redacted], and token=YOUR_TOKEN.\n"
	p := experienceGenerate(t, e)
	if p.Status != "pending_review" {
		t.Fatal("generic references or credential placeholders were rejected", p)
	}
	p, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
	if err != nil || p.Status != "published" {
		t.Fatal("generic instruction approval failed", p, err)
	}
	want := "---\nname: json-check\ndescription: Inspect structured JSON configuration\n---\nRead full JSON text before validating required fields\n" + g.extraInstruction
	if err := r.WithPublication(ctx, p.Target.SourceID, p.SkillName, func(tx *skills.PublicationTx) error {
		candidate, err := tx.ReadVersion(p.CandidateVersion)
		if err == nil && string(candidate.Files()["SKILL.md"]) != want {
			t.Fatal("credential check mutated reviewed and published bytes")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
