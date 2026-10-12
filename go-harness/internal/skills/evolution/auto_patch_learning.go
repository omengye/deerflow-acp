package evolution

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func validAutoPatchLearningSource(o harness.SkillAutoPatchObservation) bool {
	x := o.LearningSource
	if x == nil {
		return true // Observations written before this independent opt-in.
	}
	validID := func(id, prefix string) bool { return strings.HasPrefix(id, prefix) && bounded(id, 128) }
	return o.Options.ExperiencePatternsEnabled && validID(x.GenerationID, "smj_") && validID(x.DiscoveryID, "sd_") &&
		x.PatternID != "" && bounded(x.PatternID, 128) && x.MatchID != "" && bounded(x.MatchID, 128) && x.PatternRevision > 0 &&
		hexDigest(x.DiscoverySHA) && hexDigest(x.PolicyIdentity) && hexDigest(x.EvidenceSHA) &&
		(x.SourceState == "valid_at_publication" || x.SourceState == "withdrawn" || x.SourceState == "unavailable")
}

// The normal approval transaction checked the live source before Publish.
// After that CAS, record only pinned digests: the original matching head must
// not be re-admitted against the newly installed version.
func autoPatchLearningSource(ctx context.Context, tx *sql.Tx, p Proposal) (*harness.SkillAutoPatchLearningSource, error) {
	var job string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&job); err != nil {
		return nil, err
	}
	r, err := readGeneration(ctx, tx, job)
	if err != nil {
		return nil, err
	}
	d, boundJob, sha, err := readDiscovery(ctx, tx, r.input.DiscoveryID)
	if err != nil {
		return nil, err
	}
	if d.kind() != "experience_pattern" || d.ReviewDraft || boundJob != job || sha != r.input.DiscoveryDigest || d.Target != p.Target || d.Base != p.Base || len(d.Evidence) < d.Options.RepeatThreshold || d.Evidence[0].Experience == nil {
		return nil, errDiscoveryEvidence
	}
	x := d.Evidence[0].Experience
	return &harness.SkillAutoPatchLearningSource{GenerationID: job, DiscoveryID: d.ID, DiscoverySHA: sha, PatternID: x.PatternID, PatternRevision: x.PatternRevision, MatchID: x.MatchID, PolicyIdentity: x.PolicyIdentity, EvidenceSHA: encodedHash(d.Evidence), SourceState: "valid_at_publication"}, nil
}

// Withdrawal after adoption does not delete the skill or manufacture a failure
// vote. Keep monitoring independently confirmed quality with the saved limits,
// even when learning or its publication opt-in is disabled on restart.
func refreshAutoPatchLearningSource(ctx context.Context, tx *sql.Tx, o *harness.SkillAutoPatchObservation) error {
	x := o.LearningSource
	if x == nil || x.SourceState == "withdrawn" {
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_learning_promotion_dependencies')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		x.SourceState = "unavailable"
		return nil
	}
	var revoked bool
	err := tx.QueryRowContext(ctx, `SELECT revoked FROM df_learning_promotion_dependencies WHERE job_id=? AND pattern_id=? AND match_id=?`, x.GenerationID, x.PatternID, x.MatchID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		x.SourceState = "unavailable"
		return nil
	}
	if err == nil && revoked {
		x.SourceState = "withdrawn"
	}
	return err
}
