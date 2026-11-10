package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type regressionSnapshot struct {
	VerificationIdentity string                    `json:"verification_identity"`
	Environment          *behaviorSnapshot         `json:"environment,omitempty"`
	Observation          string                    `json:"observation"`
	Head                 harness.SkillHead         `json:"head"`
	Target               harness.SkillTarget       `json:"target"`
	Base                 string                    `json:"base"`
	Root                 string                    `json:"root"`
	OracleDigest         string                    `json:"oracle_digest"`
	Evaluator            string                    `json:"evaluator"`
	Case                 harness.SkillBehaviorCase `json:"case"`
}

func (s *Service) queueRegressionTx(ctx context.Context, tx *skills.PublicationTx, o harness.SkillAutoPatchObservation) error {
	if s.behavior == nil || o.PolicyVersion != "skill-evolution-v2" {
		return nil
	}
	var root, prompt, expected, input, output, kind, path string
	err := tx.SQL().QueryRowContext(ctx, `SELECT v.root_id,v.prompt,v.expected,v.input_sha,v.output_sha,COALESCE(k.kind,'output_equals'),COALESCE(k.path,'') FROM df_skill_auto_patch_evidence e JOIN df_skill_task_oracles v ON v.root_id=e.root_id LEFT JOIN df_skill_task_oracle_kinds k ON k.root_id=v.root_id WHERE e.proposal_id=? AND e.outcome='failure' AND NOT EXISTS(SELECT 1 FROM df_skill_regression_checks c WHERE c.proposal_id=e.proposal_id AND json_extract(CAST(c.payload AS TEXT),'$.root')=e.root_id AND json_extract(CAST(c.payload AS TEXT),'$.verification_identity')=?) ORDER BY e.root_id LIMIT 1`, o.ProposalID, o.VerificationIdentity).Scan(&root, &prompt, &expected, &input, &output, &kind, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	baseline, err := readPublication(ctx, tx, o.BaselineRevision)
	if err != nil {
		return err
	}
	in := regressionSnapshot{VerificationIdentity: o.VerificationIdentity, Observation: o.ProposalID, Head: o.Head, Target: o.Target, Base: baseline.ContentVersion, Root: root, OracleDigest: encodedHash([]string{root, prompt, expected, input, output, kind, path}), Evaluator: s.behavior.identity, Case: harness.SkillBehaviorCase{ID: "online-regression", Kind: "targeted", Prompt: prompt, Repetitions: 2, Assertions: []harness.SkillBehaviorAssertion{{ID: "exact", Kind: kind, Path: path, Expected: expected}}}}
	raw, _ := json.Marshal(in)
	key := "regcheck_" + hash(raw)
	var exists bool
	if err = tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_regression_checks WHERE check_key=?)`, key).Scan(&exists); err != nil || exists {
		return err
	}
	job, err := s.generation.store.EnqueueTx(ctx, tx.SQL(), skillmaintenance.Request{Kind: skillmaintenance.SkillBehavior, SourceIdentity: o.Target.SourceIdentity, Name: o.Target.Name, Key: key, Reference: key, InputDigest: hash(raw)})
	if err != nil {
		return err
	}
	_, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_regression_checks VALUES(?,?,?,?,?,'pending')`, key, job.ID, o.ProposalID, raw, hash(raw))
	return err
}

func (s *Service) regressionInputTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) (regressionSnapshot, error) {
	var in regressionSnapshot
	var raw []byte
	var sha string
	err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_skill_regression_checks WHERE job_id=? AND check_key=?`, job.ID, job.Request.Reference).Scan(&raw, &sha)
	if err != nil {
		return in, err
	}
	if len(raw) > 128<<10 || hash(raw) != sha || sha != job.Request.InputDigest || json.Unmarshal(raw, &in) != nil || in.Evaluator != s.behavior.identity {
		return in, errGenerationConfiguration
	}
	o, err := readAutoPatchObservation(ctx, tx, in.Observation)
	if err != nil {
		return in, err
	}
	head, err := skills.ReadPublicationHead(ctx, tx, in.Target.SourceIdentity, in.Target.Name)
	if err != nil {
		return in, err
	}
	if o.Head != head || head != in.Head || (o.Status != "observing" && o.Status != "stable") {
		return in, harness.ErrSkillConflict
	}
	identity, err := s.publishedIdentityTx(ctx, tx, in.Observation)
	if err != nil {
		return in, err
	}
	if identity != in.VerificationIdentity {
		return in, errGenerationConfiguration
	}
	if in.Environment != nil {
		return in, s.guardPublishedEnvironmentTx(ctx, tx, in)
	}
	var root, prompt, expected, input, output, kind, path string
	err = tx.QueryRowContext(ctx, `SELECT o.root_id,o.prompt,o.expected,o.input_sha,o.output_sha,COALESCE(k.kind,'output_equals'),COALESCE(k.path,'') FROM df_skill_task_oracles o LEFT JOIN df_skill_task_oracle_kinds k ON k.root_id=o.root_id JOIN harness_learning_controls c ON c.session_id=o.session_id AND c.epoch=o.epoch AND c.contribute=1 WHERE o.root_id=?`, in.Root).Scan(&root, &prompt, &expected, &input, &output, &kind, &path)
	if err != nil {
		return in, err
	}
	if encodedHash([]string{root, prompt, expected, input, output, kind, path}) != in.OracleDigest {
		return in, harness.ErrPermissionDenied
	}
	return in, nil
}

func (s *Service) runRegression(ctx context.Context, job skillmaintenance.Job) (string, bool) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return generationFailure(err)
	}
	in, err := s.regressionInputTx(ctx, tx, job)
	_ = tx.Rollback()
	if err != nil {
		return generationFailure(err)
	}
	basePassed, candidatePassed := true, true
	for rep := 1; rep <= in.Case.Repetitions; rep++ {
		for _, variant := range []string{"base", "candidate"} {
			key := resultKey(in.Case.ID, variant, rep)
			var result harness.SkillBehaviorResult
			var raw []byte
			var sha string
			err = s.db.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_skill_regression_results WHERE job_id=? AND result_key=?`, job.ID, key).Scan(&raw, &sha)
			if errors.Is(err, sql.ErrNoRows) {
				version := in.Base
				if variant == "candidate" {
					version = in.Head.Version
				}
				files := map[string][]byte{}
				if version != "" {
					err = s.registry.WithPublication(ctx, in.Target.SourceID, in.Target.Name, func(p *skills.PublicationTx) error {
						pkg, e := p.ReadVersion(version)
						if e == nil {
							files = pkg.Files()
						}
						return e
					})
					if err != nil {
						return generationFailure(err)
					}
				}
				input := harness.SkillBehaviorInput{EvaluationID: job.Request.Reference, CaseID: in.Case.ID, Variant: variant, Version: version, SkillName: in.Target.Name, Repetition: rep, Files: files, Case: in.Case}
				if skillbehavior.HasScripts(files) && !skillbehavior.RuntimeReady(ctx) {
					if err = s.generation.store.ObserveDependency(ctx, "blocked_runtime", "unavailable"); err != nil {
						return generationFailure(err)
					}
					return "blocked_runtime", false
				}
				result, err = s.behavior.evaluator.Evaluate(ctx, input)
				if err != nil {
					return generationFailure(err)
				}
				if err = validateBehaviorResult(result, input); err != nil {
					return generationFailure(err)
				}
				if err = validateTriggerCatalog(result, input); err != nil {
					return generationFailure(err)
				}
				raw, _ = json.Marshal(result)
				sha = hash(raw)
				tx, err = s.db.BeginTx(ctx, nil)
				if err != nil {
					return generationFailure(err)
				}
				if err = skillmaintenance.GuardTx(ctx, tx, job.ID); err == nil {
					_, err = s.regressionInputTx(ctx, tx, job)
				}
				retryMeasurement := false
				if err == nil {
					retryMeasurement, err = recordMeasurementAttemptTx(ctx, tx, job.ID, key, result)
				}
				if err == nil && !retryMeasurement {
					_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_regression_results VALUES(?,?,?,?)`, job.ID, key, raw, sha)
				}
				if err == nil {
					err = tx.Commit()
				}
				_ = tx.Rollback()
				if err != nil {
					return generationFailure(err)
				}
				if retryMeasurement {
					return "model_failed", true
				}
			} else if err != nil {
				return generationFailure(err)
			} else if hash(raw) != sha || json.Unmarshal(raw, &result) != nil {
				return "evidence_changed", false
			}
			version := in.Base
			if variant == "candidate" {
				version = in.Head.Version
			}
			bound := harness.SkillBehaviorInput{EvaluationID: job.Request.Reference, CaseID: in.Case.ID, Variant: variant, Version: version, SkillName: in.Target.Name, Repetition: rep, Case: in.Case}
			if err = validateBehaviorResult(result, bound); err != nil {
				return generationFailure(err)
			}
			if in.Case.Kind == "trigger_positive" || in.Case.Kind == "trigger_negative" {
				if version != "" {
					err = s.registry.WithPublication(ctx, in.Target.SourceID, in.Target.Name, func(p *skills.PublicationTx) error {
						pkg, e := p.ReadVersion(version)
						if e == nil {
							bound.Files = pkg.Files()
						}
						return e
					})
					if err != nil {
						return generationFailure(err)
					}
				}
				if err = validateTriggerCatalog(result, bound); err != nil {
					return generationFailure(err)
				}
			}
			if result.ErrorCode != "" && result.ErrorCode != "assertion_failed" {
				if _, err = s.db.ExecContext(ctx, `UPDATE df_skill_regression_checks SET state='measurement_failed' WHERE job_id=? AND state='pending'`, job.ID); err != nil {
					return generationFailure(err)
				}
				return "model_failed", false
			}
			if variant == "base" {
				basePassed = basePassed && result.Passed
			} else {
				candidatePassed = candidatePassed && result.Passed
			}
		}
	}
	state := "passed"
	if in.Environment != nil && candidatePassed {
		state = "environment_passed"
	} else if !basePassed {
		state = "baseline_failed"
	} else if !candidatePassed {
		state = "confirmed"
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return generationFailure(err)
	}
	defer tx.Rollback()
	if err = skillmaintenance.GuardTx(ctx, tx, job.ID); err == nil {
		_, err = s.regressionInputTx(ctx, tx, job)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE df_skill_regression_checks SET state=? WHERE job_id=?`, state, job.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	return generationFailure(err)
}
