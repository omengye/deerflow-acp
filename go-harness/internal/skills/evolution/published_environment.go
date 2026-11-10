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

// Published content stays available while a changed environment is assessed.
// Reassessment has its own durable jobs and cannot mutate publication history,
// replay generation, or manufacture online observations.
func (s *Service) publishedEnvironmentTx(ctx context.Context, pub *skills.PublicationTx, o *harness.SkillAutoPatchObservation) (bool, error) {
	if o.PolicyVersion != "skill-evolution-v2" {
		o.VerificationState = "historical"
		return true, nil
	}
	if s.behavior == nil || s.generation == nil {
		o.VerificationState = "unavailable"
		return false, nil
	}
	old, err := readBehavior(ctx, pub.SQL(), o.ProposalID)
	if err != nil {
		return false, err
	}
	identity, err := s.publishedIdentityTx(ctx, pub.SQL(), o.ProposalID)
	if errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, errDiscoveryEvidence) {
		o.VerificationState = "source_withdrawn"
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if old.input.Derived {
		p, e := readProposal(ctx, pub.SQL(), o.ProposalID)
		if e != nil {
			return false, e
		}
		if e = s.validateDerivedSuiteTx(ctx, pub.SQL(), p, old.input); e != nil && !errors.Is(e, errGenerationConfiguration) {
			if errors.Is(e, errDiscoveryEvidence) || errors.Is(e, harness.ErrPermissionDenied) {
				o.VerificationState = "source_withdrawn"
				return false, nil
			}
			return false, e
		}
		_, catalog, e := s.behaviorCompetitorsTx(ctx, pub.SQL(), p)
		if e != nil {
			return false, e
		}
		if old.input.EvaluatorIdentity == s.behavior.identity && old.input.ValidatorIdentity == s.generation.reviewIdentity && catalog == old.input.CompetitorsDigest {
			o.VerificationState, o.VerificationIdentity = "current", identity
			return true, nil
		}
	} else if old.input.EvaluatorIdentity == s.behavior.identity && old.input.ValidatorIdentity == s.generation.reviewIdentity && old.input.SuiteDigest == s.behavior.suites[behaviorKey(o.Target)].digest {
		o.VerificationState, o.VerificationIdentity = "current", identity
		return true, nil
	}
	o.VerificationState, o.VerificationIdentity = "stale", identity
	p, err := readProposal(ctx, pub.SQL(), o.ProposalID)
	if err != nil {
		return false, err
	}
	in := s.newBehaviorSnapshot(p)
	if old.input.Derived {
		if err = s.deriveBehaviorCasesTx(ctx, pub.SQL(), p, &in); err != nil {
			if errors.Is(err, errDiscoveryEvidence) || errors.Is(err, harness.ErrPermissionDenied) {
				o.VerificationState = "source_withdrawn"
				return false, nil
			}
			if errors.Is(err, skillmaintenance.ErrBlockedEvidence) {
				o.VerificationState = "blocked_evidence"
				return false, nil
			}
			return false, err
		}
	}
	if in.Suite == nil {
		o.VerificationState = "blocked_evidence"
		return false, nil
	}
	if err = persistBehaviorCases(ctx, pub.SQL(), &in); err != nil {
		return false, err
	}
	in.ID = "published_" + encodedHash([]string{o.ProposalID, identity, in.SuiteDigest, in.CaseRoot})
	allPassed, count := true, 0
	measurementFailed := false
	err = walkBehaviorCases(ctx, pub.SQL(), in, func(c harness.SkillBehaviorCase) error {
		count++
		snap := regressionSnapshot{VerificationIdentity: identity, Environment: &in, Observation: o.ProposalID, Head: o.Head, Target: o.Target, Base: in.Base.Version, Evaluator: s.behavior.identity, Case: c}
		raw, _ := json.Marshal(snap)
		key := "regcheck_" + hash(raw)
		var state string
		e := pub.SQL().QueryRowContext(ctx, `SELECT state FROM df_skill_regression_checks WHERE check_key=?`, key).Scan(&state)
		if e == sql.ErrNoRows {
			job, e := s.generation.store.EnqueueTx(ctx, pub.SQL(), skillmaintenance.Request{Kind: skillmaintenance.SkillBehavior, SourceIdentity: o.Target.SourceIdentity, Name: o.Target.Name, Key: key, Reference: key, InputDigest: hash(raw)})
			if e != nil {
				return e
			}
			_, e = pub.SQL().ExecContext(ctx, `INSERT INTO df_skill_regression_checks VALUES(?,?,?,?,?,'pending')`, key, job.ID, o.ProposalID, raw, hash(raw))
			if e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		allPassed = allPassed && state == "environment_passed"
		measurementFailed = measurementFailed || state == "measurement_failed"
		var exhausted bool
		if err := pub.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_regression_checks c JOIN df_skill_maintenance_jobs j ON j.id=c.job_id WHERE c.check_key=? AND j.state IN ('failed','cancelled'))`, key).Scan(&exhausted); err != nil {
			return err
		}
		measurementFailed = measurementFailed || exhausted
		return nil
	})
	if err != nil {
		return false, err
	}
	if allPassed && count > 0 && skillbehavior.HasScripts(inputFilesForReport(ctx, pub.SQL(), p)) {
		covered, e := publishedScriptsCovered(ctx, pub.SQL(), p, identity)
		if e != nil {
			return false, e
		}
		if !covered {
			o.VerificationState = "blocked_runtime"
			return false, nil
		}
	}
	if measurementFailed {
		o.VerificationState = "measurement_failed"
		return false, nil
	}
	if allPassed && count > 0 {
		o.VerificationState = "current"
		return true, nil
	}
	o.VerificationState = "rechecking"
	return false, nil
}

func (s *Service) guardPublishedEnvironmentTx(ctx context.Context, tx *sql.Tx, in regressionSnapshot) error {
	p, err := readProposal(ctx, tx, in.Observation)
	if err != nil {
		return err
	}
	e := *in.Environment
	if e.ProposalID != p.ID || e.Target != p.Target || e.Base != p.Base || e.CandidateVersion != p.CandidateVersion || e.EvaluatorIdentity != s.behavior.identity || e.ValidatorIdentity != s.generation.reviewIdentity {
		return errGenerationConfiguration
	}
	if err = s.generation.checkSource(generationRecord{input: generationSnapshot{Target: p.Target}}); err != nil {
		return err
	}
	if e.Derived {
		if err = s.validateDerivedSuiteTx(ctx, tx, p, e); err != nil {
			return err
		}
	} else if e.SuiteDigest != s.behavior.suites[behaviorKey(p.Target)].digest {
		return errGenerationConfiguration
	}
	c, err := findBehaviorCase(ctx, tx, e, in.Case.ID)
	if err != nil {
		return err
	}
	if encodedHash(*c) != encodedHash(in.Case) {
		return harness.ErrPermissionDenied
	}
	return nil
}

// A result is reusable only under the complete current host binding, including
// suite/catalog, validator, evaluator and its tool/runtime identity.
func (s *Service) publishedIdentityTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	if s.behavior == nil || s.generation == nil {
		return "", errGenerationConfiguration
	}
	old, err := readBehavior(ctx, tx, id)
	if err != nil {
		return "", err
	}
	identity := encodedHash([]string{s.behavior.bindingIdentity(old.input.Target), s.generation.reviewIdentity})
	if old.input.Derived {
		p, err := readProposal(ctx, tx, id)
		if err != nil {
			return "", err
		}
		_, catalog, err := s.behaviorCompetitorsTx(ctx, tx, p)
		if err != nil {
			return "", err
		}
		identity = encodedHash([]string{identity, catalog})
	}
	return identity, nil
}

func publishedScriptsCovered(ctx context.Context, tx *sql.Tx, p Proposal, identity string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.payload,r.payload_sha FROM df_skill_regression_results r JOIN df_skill_regression_checks c ON c.job_id=r.job_id WHERE c.proposal_id=? AND c.state='environment_passed' AND json_extract(CAST(c.payload AS TEXT),'$.verification_identity')=? AND json_extract(CAST(r.payload AS TEXT),'$.variant')='candidate'`, p.ID, identity)
	if err != nil {
		return false, err
	}
	seen := map[string]bool{}
	var proofs []harness.SkillBehaviorResult
	for rows.Next() {
		var raw []byte
		var sha string
		var result harness.SkillBehaviorResult
		if err = rows.Scan(&raw, &sha); err != nil {
			break
		}
		if len(raw) > 16<<10 || hash(raw) != sha || json.Unmarshal(raw, &result) != nil || result.Version != p.CandidateVersion {
			err = errDiscoveryEvidence
			break
		}
		for _, proof := range result.Scripts {
			if !validScriptExecution(proof) || proof.ExitCode != 0 {
				err = errDiscoveryEvidence
				break
			}
			if !seen[proof.Path] {
				seen[proof.Path] = true
				proofs = append(proofs, harness.SkillBehaviorResult{Variant: "candidate", Scripts: []harness.SkillScriptExecution{proof}})
			}
		}
		if err != nil {
			break
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	return scriptsCovered(inputFilesForReport(ctx, tx, p), proofs), nil
}
