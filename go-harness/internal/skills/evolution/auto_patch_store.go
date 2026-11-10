package evolution

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type autoPatchHost struct {
	options   harness.SkillAutoPatchOptions
	builtin   []string
	lifecycle collectorLifecycle
	now       func() time.Time
}
type autoPatchContextKey struct{}

var errAutoPatchReview = errors.New("candidate is waiting for automatic adoption requirements")

type autoPatchDecisionError struct{ reason string }

func (e autoPatchDecisionError) Error() string {
	return fmt.Sprintf("%s: %s", errAutoPatchReview, e.reason)
}
func (e autoPatchDecisionError) Unwrap() error { return errAutoPatchReview }
func autoPatchDecision(reason string) error    { return autoPatchDecisionError{reason: reason} }

func validAutoPatchOptions(o harness.SkillAutoPatchOptions) bool {
	return o.MaxChangedLines >= 1 && o.MaxChangedLines <= 200 && o.ObservationHours >= 1 && o.ObservationHours <= 720 && o.MinObservations >= 1 && o.MinObservations <= 100 && o.RollbackFailureThreshold >= 1 && o.RollbackFailureThreshold <= 100
}

func migrateAutoPatch(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_auto_patch_observations(
 proposal_id TEXT PRIMARY KEY REFERENCES df_skill_proposals(id),source_key TEXT NOT NULL,name TEXT NOT NULL,
 status TEXT NOT NULL,started_ns INTEGER NOT NULL,payload BLOB NOT NULL,payload_sha TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_skill_auto_patch_active ON df_skill_auto_patch_observations(status,proposal_id);
CREATE TABLE IF NOT EXISTS df_skill_auto_patch_scan(id INTEGER PRIMARY KEY CHECK(id=1),publish_cursor TEXT NOT NULL,monitor_cursor TEXT NOT NULL,last_error_code TEXT NOT NULL);
INSERT OR IGNORE INTO df_skill_auto_patch_scan VALUES(1,'','','');`)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS df_skill_auto_patch_decisions(
 proposal_id TEXT PRIMARY KEY REFERENCES df_skill_proposals(id),state_version INTEGER NOT NULL,
 policy_identity TEXT NOT NULL,reason TEXT NOT NULL);`); err != nil {
		return err
	}
	return migrateAutoPatchEvidence(ctx, tx)
}

func (s *Service) ConfigureAutoPatch(options harness.SkillAutoPatchOptions) error {
	if s.autoPatch != nil || s.generation == nil || s.behavior == nil || !validAutoPatchOptions(options) {
		return invalid("auto_patch requires generation, behavior evaluation and bounded options")
	}
	builtin, err := s.registry.BuiltinSources()
	if err != nil {
		return err
	}
	s.autoPatch = &autoPatchHost{options: options, builtin: builtin, now: time.Now}
	return nil
}

// The edit distance counts inserted/deleted lines, with an upper bound. This
// avoids treating our deliberately full-file review diff as a whole-file edit.
func changedTextLines(a, b []byte, limit int) int {
	x := strings.Split(strings.ReplaceAll(string(a), "\r\n", "\n"), "\n")
	y := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for len(x) > 0 && len(y) > 0 && x[0] == y[0] {
		x = x[1:]
		y = y[1:]
	}
	for len(x) > 0 && len(y) > 0 && x[len(x)-1] == y[len(y)-1] {
		x = x[:len(x)-1]
		y = y[:len(y)-1]
	}
	if len(strings.Join(x, "\n"))+len(strings.Join(y, "\n")) > 16<<10 {
		return limit + 1
	}
	if len(x)-len(y) > limit || len(y)-len(x) > limit {
		return limit + 1
	}
	// Myers' bounded shortest edit search; unchanged spans cost no edits.
	v := map[int]int{1: 0}
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			at := 0
			if k == -d || k != d && v[k-1] < v[k+1] {
				at = v[k+1]
			} else {
				at = v[k-1] + 1
			}
			bt := at - k
			for at < len(x) && bt < len(y) && at >= 0 && bt >= 0 && x[at] == y[bt] {
				at++
				bt++
			}
			v[k] = at
			if at >= len(x) && bt >= len(y) {
				return d
			}
		}
	}
	return limit + 1
}

func (s *Service) guardAutoPatch(ctx context.Context, tx *skills.PublicationTx, p Proposal, input ValidationInput) error {
	if enabled, _ := ctx.Value(autoPatchContextKey{}).(bool); !enabled {
		return nil
	}
	learningCandidate := p.Trigger.Type == "experience_pattern"
	if containsScripts(input.CandidateFiles) && !p.Evaluation.IsolatedScripts {
		return autoPatchDecision("blocked_runtime")
	}
	a := s.autoPatch
	if p.Origin == "automatic" && p.Evaluation.BehaviorReport != nil && p.Evaluation.BehaviorReport.HoldoutCases < 1 {
		return autoPatchDecision("blocked_evidence")
	}
	if a != nil && slices.Contains(a.builtin, p.Target.SourceID) {
		return autoPatchDecision("blocked_scope_or_coverage")
	}
	if learningCandidate && (a == nil || s.discovery == nil || s.experienceSource == nil || !s.experienceSource.PromotionEnabled()) {
		return autoPatchDecision("learning_authorization_required")
	}
	if a == nil || p.Base.Exists && !p.Base.Enabled || p.OverlayBase != nil || p.Origin != "automatic" || p.Action != "edit" && p.Action != "patch" && p.Action != "create" || p.Evaluation.Security != "passed" || p.Evaluation.Behavior != "passed" || p.Evaluation.BehaviorReport == nil || p.Evaluation.BehaviorReport.Regressions != 0 || p.Evaluation.BehaviorReport.TargetedCases < 1 || p.Evaluation.BehaviorReport.RegressionCases < 1 {
		return autoPatchDecision("blocked_scope_or_coverage")
	}
	// All automatic origins need actual improvement, not merely two passing
	// variants. Metadata rewrites use the same stricter trigger comparison.
	contentAllowed := learningAutoPatchContentAllowed(input.BaseFiles["SKILL.md"], input.CandidateFiles["SKILL.md"], *p.Evaluation.BehaviorReport)
	if !p.Base.Exists {
		contentAllowed = learningAutoPatchImproved(*p.Evaluation.BehaviorReport) && p.Evaluation.BehaviorReport.TriggerPositiveCases > 0 && p.Evaluation.BehaviorReport.TriggerNegativeCases > 0
	}
	if !contentAllowed {
		return autoPatchDecision("improvement_not_proven")
	}
	changed := 0
	for _, path := range p.ChangedFiles {
		changed += changedTextLines(input.BaseFiles[path], input.CandidateFiles[path], a.options.MaxChangedLines)
	}
	if changed < 1 || changed > a.options.MaxChangedLines {
		return autoPatchDecision("change_size_exceeded")
	}
	var jobID string
	if err := tx.SQL().QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&jobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return autoPatchDecision("generation_binding_invalid")
		}
		return err
	}
	r, err := readGeneration(ctx, tx.SQL(), jobID)
	if err != nil {
		return err
	}
	generationComplete := r.job.State == "completed"
	if !generationComplete && r.job.State == "failed" && r.job.ErrorCode == "configuration_changed" {
		if err = tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_behavior_evaluations e JOIN df_skill_maintenance_jobs j ON j.id=e.job_id WHERE e.proposal_id=? AND e.manual=1 AND j.state='completed')`, p.ID).Scan(&generationComplete); err != nil {
			return err
		}
	}
	if !generationComplete || r.proposal != p.ID || r.candidate != p.CandidateVersion || r.input.Target != p.Target || r.input.Request.Base != p.Base {
		return autoPatchDecision("generation_binding_invalid")
	}
	if err = s.generation.checkSource(r); err != nil {
		return autoPatchDecision("source_authorization_required")
	}
	// Generation identity remains provenance. Current review and evaluation
	// identities are enforced by guardBehaviorReport, including re-evaluation.
	if r.input.DiscoveryID != "" {
		d, _, _, err := readDiscovery(ctx, tx.SQL(), r.input.DiscoveryID)
		if err != nil {
			return err
		}
		if (d.kind() == "experience_pattern") != learningCandidate || learningCandidate && (d.Options != s.discovery.options ||
			d.HostAdjudicated || d.ReviewDraft || len(d.Evidence) < d.Options.RepeatThreshold || d.Evidence[0].Experience == nil || strings.HasPrefix(d.Evidence[0].Experience.MatchID, "ladj_")) {
			if d.ReviewDraft {
				return autoPatchDecision("waiting_evidence")
			}
			return autoPatchDecision("blocked_evidence")
		}
	} else if learningCandidate {
		return autoPatchDecision("blocked_evidence")
	}
	var count int
	if err = tx.SQL().QueryRowContext(ctx, `SELECT count(*) FROM df_skill_auto_patch_observations WHERE source_key=? AND name=? AND status='observing'`, p.Target.SourceIdentity, p.Target.Name).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return autoPatchDecision("waiting_observation")
	}
	return nil
}

func writeAutoPatchObservation(ctx context.Context, tx *sql.Tx, o harness.SkillAutoPatchObservation, insert bool) error {
	payload, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if insert {
		_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_auto_patch_observations(proposal_id,source_key,name,status,started_ns,payload,payload_sha) VALUES(?,?,?,?,?,?,?)`, o.ProposalID, o.Target.SourceIdentity, o.Target.Name, o.Status, o.StartedAt.UnixNano(), payload, hash(payload))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE df_skill_auto_patch_observations SET status=?,payload=?,payload_sha=? WHERE proposal_id=?`, o.Status, payload, hash(payload), o.ProposalID)
	}
	return err
}

func readAutoPatchObservation(ctx context.Context, db rowReader, id string) (harness.SkillAutoPatchObservation, error) {
	var o harness.SkillAutoPatchObservation
	var data []byte
	var source, name, status, digest string
	var started int64
	err := db.QueryRowContext(ctx, `SELECT source_key,name,status,started_ns,payload,payload_sha FROM df_skill_auto_patch_observations WHERE proposal_id=?`, id).Scan(&source, &name, &status, &started, &data, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return o, harness.ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if hash(data) != digest || json.Unmarshal(data, &o) != nil || o.ProposalID != id || o.Target.SourceIdentity != source || o.Target.Name != name || o.Status != status || o.StartedAt.UnixNano() != started || o.PublicationRevision < 2 || o.BaselineRevision != o.PublicationRevision-1 || !o.Head.Exists || !o.Head.Enabled || !validAutoPatchOptions(o.Options) || !o.Deadline.Equal(o.StartedAt.Add(time.Duration(o.Options.ObservationHours)*time.Hour)) || !validAutoPatchLearningSource(o) {
		return o, invalid("auto_patch observation is inconsistent")
	}
	switch o.Status {
	case "observing", "stable", "passed", "insufficient_evidence", "rolled_back", "superseded", "withdrawn":
	default:
		return o, invalid("auto_patch observation status is inconsistent")
	}
	return o, nil
}

func (s *Service) attachAutoPatchObservation(ctx context.Context, tx *skills.PublicationTx, p Publication, proposal Proposal) error {
	if enabled, _ := ctx.Value(autoPatchContextKey{}).(bool); !enabled {
		return nil
	}
	previous, err := readPublication(ctx, tx, p.Revision-1)
	if err != nil {
		return err
	}
	if previous.Head != p.Previous {
		return harness.ErrSkillConflict
	}
	now := s.autoPatch.now().UTC()
	o := harness.SkillAutoPatchObservation{PolicyVersion: "skill-evolution-v2", ProposalID: p.ProposalID, Target: p.Target, PublicationRevision: p.Revision, BaselineRevision: previous.Revision, Head: p.Head, Options: s.autoPatch.options, Status: "observing", StartedAt: now, Deadline: now.Add(time.Duration(s.autoPatch.options.ObservationHours) * time.Hour), UpdatedAt: now}
	if proposal.Trigger.Type == "experience_pattern" {
		o.LearningSource, err = autoPatchLearningSource(ctx, tx.SQL(), proposal)
		if err != nil {
			return err
		}
	}
	workspace, err := s.automaticWorkspace(ctx, tx.SQL(), proposal)
	if err != nil {
		return err
	}
	_ = workspace // Scope and immutable route were committed by PublishAutomatic.
	// Remove any already prepared export before the publication transaction commits.
	if _, err = tx.SQL().ExecContext(ctx, `DELETE FROM df_skill_exports WHERE source_key=? AND name=? AND state_version=?`, p.Target.SourceIdentity, p.Target.Name, p.Head.StateVersion); err != nil {
		return err
	}
	return writeAutoPatchObservation(ctx, tx.SQL(), o, true)
}

// Execution-only evidence cannot authorize a metadata rewrite. This exact
// comparison remains the body-only path, with CRLF normalized for portability.
func sameAutoPatchFrontmatter(base, candidate []byte) bool {
	frontmatter := func(data []byte) []byte {
		text := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		if !bytes.HasPrefix(text, []byte("---\n")) {
			return nil
		}
		end := bytes.Index(text[4:], []byte("\n---\n"))
		if end < 0 || end > 16<<10 {
			return nil
		}
		return text[:4+end+5]
	}
	a, b := frontmatter(base), frontmatter(candidate)
	return a != nil && b != nil && bytes.Equal(a, b)
}

func learningAutoPatchImproved(report harness.SkillBehaviorReport) bool {
	if report.State != "passed" || report.TargetedCases < 1 || report.RegressionCases < 1 || report.Regressions != 0 || report.CandidateFailures > 0 || report.MeasurementErrors > 0 {
		return false
	}
	if report.TotalComparisons <= len(report.Results) {
		improved := false
		for _, c := range report.Results {
			if c.BaseErrorCode != "" && c.BaseErrorCode != "assertion_failed" || !c.CandidatePassed {
				return false
			}
			if c.Kind == "targeted" && !c.BasePassed && c.BaseErrorCode == "assertion_failed" && c.CandidatePassed {
				improved = true
			}
		}
		return improved
	}
	return report.TargetedImprovements > 0
}
