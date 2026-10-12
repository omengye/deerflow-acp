package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func validSourceRequest(r harness.SkillLearningSourceRequest) bool {
	return validText(r.ID, 128, true) && validText(r.ExperienceID, 128, true) && strings.HasPrefix(r.ExperienceID, "lexp_") && r.ExpectedRevision > 0 &&
		(r.Kind == "experience" && strings.HasPrefix(r.ID, "lexp_") && r.Scope == "session" && r.ID == r.ExperienceID ||
			r.Kind == "pattern" && strings.HasPrefix(r.ID, "lpat_") && (r.Scope == "session" || r.Scope == "workspace"))
}

// One selected member keeps the response bounded and preserves attribution.
// This is not a live match, promotion or whole-pattern quality certification.
func (s *Store) Source(ctx context.Context, actor string, r harness.SkillLearningSourceRequest) (harness.SkillLearningSource, error) {
	var out harness.SkillLearningSource
	if !validSourceRequest(r) || !validText(actor, 256, true) {
		return out, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var member *experienceDependency
	if r.Kind == "pattern" {
		p, err := s.reviewScopeTx(ctx, tx, actor, r.ID, r.Scope, r.ExpectedRevision)
		if err != nil {
			return out, err
		}
		var revision int64
		if err = tx.QueryRowContext(ctx, `SELECT current_revision FROM df_learning_patterns WHERE id=?`, r.ID).Scan(&revision); err != nil {
			return out, err
		}
		if revision != r.ExpectedRevision {
			return out, harness.ErrSkillConflict
		}
		for _, dep := range p.Members {
			if dep.ID == r.ExperienceID {
				copy := dep
				member = &copy
				break
			}
		}
		if member == nil {
			return out, harness.ErrPermissionDenied
		}
	}
	var data []byte
	var e harness.SkillExperience
	var sha, session, episode, extraction string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT payload,payload_sha,session_id,episode_id,extraction_id,revision
 FROM df_learning_experiences WHERE id=?`, r.ExperienceID).Scan(&data, &sha, &session, &episode, &extraction, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = harness.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if r.Kind == "experience" && session != actor || member != nil && (member.Session != session || member.Episode != episode || member.Extraction != extraction || member.SHA != sha) {
		return out, harness.ErrPermissionDenied
	}
	if r.Kind == "experience" && revision != r.ExpectedRevision {
		return out, harness.ErrSkillConflict
	}
	if len(data) > 64<<10 || digest(data) != sha || json.Unmarshal(data, &e) != nil || e.ID != r.ExperienceID || e.SessionID != session || e.EpisodeID != episode || e.Revision != revision || revision != 1 || e.Origin != "model_extracted" || e.Quality != "unassessed" {
		return out, invalid("inconsistent source experience")
	}
	snap, inputSHA, resultSHA, err := s.sourceExtractionTx(ctx, tx, extraction)
	if err != nil {
		return out, err
	}
	if snap.Session != session || snap.Episode != episode || member != nil && (member.SourceSHA != inputSHA || member.ResultSHA != resultSHA || member.Epoch != snap.Epoch) {
		return out, harness.ErrPermissionDenied
	}
	out = harness.SkillLearningSource{SessionID: actor, Source: r, SourceSessionID: session, EpisodeID: episode,
		ConsentEpoch: snap.Epoch, ExtractionID: extraction, InputSHA: inputSHA, ResultSHA: resultSHA,
		PolicyVersion: PolicyVersion, ExternalContextPolicy: "hints_only", SourceState: "checked_at_read", Quality: "unassessed", References: []harness.SkillLearningReference{}}
	refs := make(map[string]harness.SkillLearningReference, len(snap.View.References))
	for _, ref := range snap.View.References {
		refs[ref.ID] = ref
	}
	for _, id := range e.Candidate.References {
		ref, found := refs[id]
		if !found {
			return harness.SkillLearningSource{}, invalid("missing cited source reference")
		}
		out.References = append(out.References, ref)
	}
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > 512<<10 || len(out.References) < 1 || len(out.References) > 32 {
		return harness.SkillLearningSource{}, invalid("source inspection exceeds bounds")
	}
	if err = tx.Commit(); err != nil {
		return harness.SkillLearningSource{}, err
	}
	return out, nil
}

func (s *Store) sourceExtractionTx(ctx context.Context, tx *sql.Tx, id string) (extractionSnapshot, string, string, error) {
	var snap extractionSnapshot
	var data []byte
	var sha, resultSHA, policy, model, session, episode, jobID string
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha,result_sha,policy_version,model_identity,session_id,episode_id,epoch,job_id
 FROM df_learning_extractions WHERE id=?`, id).Scan(&data, &sha, &resultSHA, &policy, &model, &session, &episode, &epoch, &jobID)
	if err != nil {
		return snap, sha, resultSHA, err
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, jobID)
	if err != nil {
		return snap, sha, resultSHA, err
	}
	if len(data) > (2<<20)+(32<<10) || digest(data) != sha || json.Unmarshal(data, &snap) != nil || policy != PolicyVersion || model == "" || resultSHA == "" ||
		snap.ID != id || snap.Session != session || snap.Episode != episode || snap.Epoch != epoch || snap.MaxDailyModelCalls < 1 || snap.MaxDailyTokens < 1 ||
		job.State != "completed" || job.Request.Kind != skillmaintenance.ExperienceExtract || job.Request.OwnerKind != skillmaintenance.WorkspaceOwner ||
		job.Request.OwnerKey != snap.Owner || job.Request.Reference != id || job.Request.Key != id || job.Request.InputDigest != sha ||
		id != extractionID(snap, model) {
		return snap, sha, resultSHA, invalid("inconsistent inspection extraction identity")
	}
	workspace, err := sessionWorkspaceTx(ctx, tx, session)
	if err != nil {
		return snap, sha, resultSHA, err
	}
	if workspace != snap.Owner {
		return snap, sha, resultSHA, harness.ErrPermissionDenied
	}
	if err = s.verifySnapshotEvidenceTx(ctx, tx, snap, false); err == nil {
		err = s.verifyResultTx(ctx, tx, snap, resultSHA)
	}
	return snap, sha, resultSHA, err
}
