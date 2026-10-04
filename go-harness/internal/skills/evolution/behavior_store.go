package evolution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type behaviorBinding struct {
	target harness.SkillTarget
	suite  harness.SkillBehaviorSuite
	digest string
}
type behaviorHost struct {
	evaluator harness.SkillBehaviorEvaluator
	identity  string
	suites    map[string]behaviorBinding
}
type behaviorSnapshot struct {
	ID                string                      `json:"id"`
	ProposalID        string                      `json:"proposal_id"`
	Target            harness.SkillTarget         `json:"target"`
	Base              harness.SkillHead           `json:"base"`
	CandidateVersion  string                      `json:"candidate_version"`
	OverlayBase       *harness.SkillRef           `json:"overlay_base,omitempty"`
	ValidatorIdentity string                      `json:"validator_identity"`
	EvaluatorIdentity string                      `json:"evaluator_identity"`
	Suite             *harness.SkillBehaviorSuite `json:"suite,omitempty"`
	SuiteDigest       string                      `json:"suite_digest,omitempty"`
}
type behaviorRecord struct {
	input   behaviorSnapshot
	jobID   string
	manual  bool
	content *Report
	report  *harness.SkillBehaviorReport
}

func migrateBehavior(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_behavior_evaluations(
 id TEXT PRIMARY KEY,proposal_id TEXT NOT NULL UNIQUE REFERENCES df_skill_proposals(id),
 job_id TEXT NOT NULL REFERENCES df_skill_maintenance_jobs(id),manual INTEGER NOT NULL,
 source_key TEXT NOT NULL,name TEXT NOT NULL,payload BLOB NOT NULL,payload_sha TEXT NOT NULL,
 content_report BLOB,content_sha TEXT NOT NULL DEFAULT '',report BLOB,report_sha TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS df_skill_behavior_job ON df_skill_behavior_evaluations(job_id);
CREATE UNIQUE INDEX IF NOT EXISTS df_skill_behavior_manual_job ON df_skill_behavior_evaluations(job_id) WHERE manual=1;
CREATE TABLE IF NOT EXISTS df_skill_behavior_results(
 evaluation_id TEXT NOT NULL REFERENCES df_skill_behavior_evaluations(id),case_id TEXT NOT NULL,variant TEXT NOT NULL,repetition INTEGER NOT NULL,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,PRIMARY KEY(evaluation_id,case_id,variant,repetition));`)
	return err
}

func behaviorKey(target harness.SkillTarget) string { return target.SourceIdentity + "/" + target.Name }

func (s *Service) BehaviorEnabled() bool { return s.behavior != nil }

// ConfigureBehavior fixes trusted fixture/oracle content before workers start.
// A behavioral evaluator has no role in authorizing publication.
func (s *Service) ConfigureBehavior(evaluator harness.SkillBehaviorEvaluator, options harness.SkillBehaviorOptions) error {
	if s.behavior != nil || s.generation == nil || evaluator == nil || evaluator.Identity() == "" || !bounded(evaluator.Identity(), 128) || len(options.Suites) == 0 || len(options.Suites) > 32 {
		return invalid("behavior requires configured generation, evaluator and bounded host suites")
	}
	h := &behaviorHost{evaluator: evaluator, identity: evaluator.Identity(), suites: map[string]behaviorBinding{}}
	for _, suite := range options.Suites {
		digest, err := skillbehavior.Digest(suite)
		if err != nil {
			return err
		}
		target, ok := s.generation.allowed[suite.SourceID]
		if !ok {
			return harness.ErrPermissionDenied
		}
		target.Name = suite.Name
		key := behaviorKey(target)
		if _, exists := h.suites[key]; exists {
			return invalid("duplicate behavior target")
		}
		encoded, err := json.Marshal(suite)
		if err != nil || len(encoded) > 2<<20 {
			return invalid("behavior suite exceeds bounds")
		}
		var fixed harness.SkillBehaviorSuite
		if json.Unmarshal(encoded, &fixed) != nil {
			return invalid("behavior suite cannot be fixed")
		}
		h.suites[key] = behaviorBinding{target: target, suite: fixed, digest: digest}
	}
	s.behavior = h
	s.generation.behavior = h
	return nil
}

func (h *behaviorHost) bindingIdentity(target harness.SkillTarget) string {
	if h == nil {
		return ""
	}
	b := h.suites[behaviorKey(target)]
	return encodedHash([]string{"skill-behavior-binding-v1", h.identity, b.digest})
}

func (s *Service) newBehaviorSnapshot(p Proposal) behaviorSnapshot {
	in := behaviorSnapshot{ID: "sbe_" + rand.Text(), ProposalID: p.ID, Target: p.Target, Base: p.Base, CandidateVersion: p.CandidateVersion, OverlayBase: p.OverlayBase, ValidatorIdentity: s.generation.reviewIdentity, EvaluatorIdentity: s.behavior.identity}
	if b, ok := s.behavior.suites[behaviorKey(p.Target)]; ok {
		fixed := b.suite
		in.Suite = &fixed
		in.SuiteDigest = b.digest
	}
	return in
}

func insertBehavior(ctx context.Context, tx *sql.Tx, in behaviorSnapshot, jobID string, manual bool) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_behavior_evaluations(id,proposal_id,job_id,manual,source_key,name,payload,payload_sha) VALUES(?,?,?,?,?,?,?,?)`, in.ID, in.ProposalID, jobID, manual, in.Target.SourceIdentity, in.Target.Name, payload, hash(payload))
	return err
}

func readBehavior(ctx context.Context, db rowReader, proposalID string) (behaviorRecord, error) {
	var r behaviorRecord
	var id, source, name, digest, contentSHA, reportSHA string
	var payload, content, report []byte
	err := db.QueryRowContext(ctx, `SELECT id,job_id,manual,source_key,name,payload,payload_sha,content_report,content_sha,report,report_sha FROM df_skill_behavior_evaluations WHERE proposal_id=?`, proposalID).Scan(&id, &r.jobID, &r.manual, &source, &name, &payload, &digest, &content, &contentSHA, &report, &reportSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return r, harness.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if len(payload) > 3<<20 || hash(payload) != digest || json.Unmarshal(payload, &r.input) != nil || r.input.ID != id || r.input.ProposalID != proposalID || !strings.HasPrefix(id, "sbe_") || len(id) > 128 || r.input.Target.SourceIdentity != source || r.input.Target.Name != name || !validOverlayBase(r.input.OverlayBase, r.input.Target) || r.input.CandidateVersion != "" && !hexDigest(r.input.CandidateVersion) || r.input.ValidatorIdentity == "" || r.input.EvaluatorIdentity == "" {
		return r, invalid("behavior record is inconsistent")
	}
	if r.input.Suite == nil {
		if r.input.SuiteDigest != "" {
			return r, invalid("behavior suite binding is inconsistent")
		}
	} else {
		calculated, err := skillbehavior.Digest(*r.input.Suite)
		if err != nil || calculated != r.input.SuiteDigest || r.input.Suite.SourceID != r.input.Target.SourceID || r.input.Suite.Name != r.input.Target.Name {
			return r, invalid("behavior suite binding is inconsistent")
		}
	}
	if content != nil {
		var saved Report
		if len(content) > 64<<10 || hash(content) != contentSHA || json.Unmarshal(content, &saved) != nil {
			return r, invalid("behavior content review is inconsistent")
		}
		r.content = &saved
	} else if contentSHA != "" {
		return r, invalid("behavior content digest is inconsistent")
	}
	if report != nil {
		var saved harness.SkillBehaviorReport
		if len(report) > 48<<10 || hash(report) != reportSHA || json.Unmarshal(report, &saved) != nil {
			return r, invalid("behavior report is inconsistent")
		}
		r.report = &saved
	} else if reportSHA != "" {
		return r, invalid("behavior report digest is inconsistent")
	}
	return r, nil
}

func (s *Service) queueBehaviorManual(ctx context.Context, tx *skills.PublicationTx, p *Proposal) (bool, error) {
	if s.behavior == nil {
		return false, nil
	}
	if id, _ := ctx.Value(generationContextKey{}).(string); id != "" {
		return false, nil
	}
	if err := s.generation.checkSource(generationRecord{input: generationSnapshot{Target: p.Target}}); err != nil {
		return false, err
	}
	in := s.newBehaviorSnapshot(*p)
	payload, err := json.Marshal(in)
	if err != nil {
		return false, err
	}
	job, err := s.generation.store.EnqueueTx(ctx, tx.SQL(), skillmaintenance.Request{SourceIdentity: p.Target.SourceIdentity, Name: p.Target.Name, Key: "behavior-review/" + p.ID, Reference: in.ID, InputDigest: hash(payload)})
	if err != nil {
		return false, err
	}
	if err = insertBehavior(ctx, tx.SQL(), in, job.ID, true); err != nil {
		return false, err
	}
	p.BehaviorJobID = job.ID
	saved, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	_, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_proposals SET payload=? WHERE id=?`, saved, p.ID)
	return true, err
}

func (s *Service) attachBehaviorGeneration(ctx context.Context, tx *skills.PublicationTx, p Proposal, r generationRecord) error {
	if r.input.BehaviorIdentity == "" {
		return nil
	}
	if s.behavior == nil || s.behavior.bindingIdentity(p.Target) != r.input.BehaviorIdentity {
		return errGenerationConfiguration
	}
	if prior, err := readBehavior(ctx, tx.SQL(), p.ID); err == nil {
		if prior.jobID != r.job.ID || prior.manual {
			return harness.ErrSkillConflict
		}
		return nil
	} else if !errors.Is(err, harness.ErrNotFound) {
		return err
	}
	return insertBehavior(ctx, tx.SQL(), s.newBehaviorSnapshot(p), r.job.ID, false)
}

func (s *Service) checkBehavior(ctx context.Context, tx *sql.Tx, r behaviorRecord, p Proposal) error {
	if s.behavior == nil || s.generation == nil || r.input.EvaluatorIdentity != s.behavior.identity || r.input.ValidatorIdentity != s.generation.reviewIdentity {
		return errGenerationConfiguration
	}
	if r.input.Target != p.Target || r.input.Base != p.Base || r.input.CandidateVersion != p.CandidateVersion || !skills.SameOverlayBase(r.input.OverlayBase, p.OverlayBase) {
		return harness.ErrSkillConflict
	}
	b := s.behavior.suites[behaviorKey(p.Target)]
	if b.digest != r.input.SuiteDigest {
		return errGenerationConfiguration
	}
	if err := s.generation.checkSource(generationRecord{input: generationSnapshot{Target: p.Target}}); err != nil {
		return err
	}
	head, err := skills.ReadPublicationHead(ctx, tx, p.Target.SourceIdentity, p.Target.Name)
	if err != nil {
		return err
	}
	if head != p.Base {
		return harness.ErrSkillConflict
	}
	if err = checkMaintenanceOverlay(ctx, tx, p.Target, r.input.OverlayBase); err != nil {
		return err
	}
	if p.Status != "validating" {
		return harness.ErrSkillConflict
	}
	return nil
}

// Admission runs for every model reservation, including multiple turns within
// one fixture. The imported base under a builtin overlay can change even when
// its managed publication head remains identical.
func checkMaintenanceOverlay(ctx context.Context, tx *sql.Tx, target harness.SkillTarget, expected *harness.SkillRef) error {
	var version string
	err := tx.QueryRowContext(ctx, `SELECT base_version FROM df_skill_overlays WHERE source_key=? AND name=?`, target.SourceIdentity, target.Name).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if expected == nil {
			return nil
		}
		err = tx.QueryRowContext(ctx, `SELECT last_import_version FROM df_skill_control WHERE source_key=? AND name=?`, target.SourceIdentity, target.Name).Scan(&version)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return harness.ErrSkillConflict
	}
	if err != nil {
		return err
	}
	if expected == nil || version != expected.Version {
		return harness.ErrSkillConflict
	}
	return nil
}
