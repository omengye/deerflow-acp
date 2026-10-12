package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const ResolutionPolicyVersion = "procedural-same-task-resolution-v1"

type experienceResolution struct {
	Prior    string `json:"prior"`
	Verified string `json:"verified"`
}

func projectPattern(snap patternSnapshot, members []harness.SkillExperience) (harness.SkillExperiencePattern, error) {
	resolved := map[string]bool{}
	if snap.ResolutionPolicy != "" && snap.ResolutionPolicy != ResolutionPolicyVersion || len(snap.Resolutions) > 0 && snap.ResolutionPolicy == "" || len(snap.Resolutions) >= len(snap.Members) || len(members) != len(snap.Members) {
		return harness.SkillExperiencePattern{}, invalid("invalid pattern resolution policy or bounds")
	}
	indices := map[string]int{}
	for i, dep := range snap.Members {
		indices[dep.ID] = i
	}
	for i, resolution := range snap.Resolutions {
		prior, priorFound := indices[resolution.Prior]
		verified, verifiedFound := indices[resolution.Verified]
		if !priorFound || !verifiedFound || resolution.Prior == resolution.Verified || i > 0 && resolution.Prior <= snap.Resolutions[i-1].Prior {
			return harness.SkillExperiencePattern{}, invalid("invalid pattern resolution member")
		}
		if !sameResolutionTask(snap.Members[prior], snap.Members[verified]) || members[prior].Candidate.Outcome == "observed_execution" || members[verified].Candidate.Outcome != "observed_execution" {
			return harness.SkillExperiencePattern{}, invalid("invalid pattern resolution authority")
		}
		resolved[resolution.Prior] = true
	}
	p := snap.Pattern
	return buildPatternResolved(p.ID, p.SessionID, p.Revision, p.RepresentativeID, snap.Members, members, p.ConflictsWith, p.EvidenceState == "ambiguous", p.UpdatedAt, resolved)
}

func sameResolutionTask(prior, verified experienceDependency) bool {
	return prior.Session == verified.Session && prior.Episode == verified.Episode && prior.Epoch == verified.Epoch && prior.Independent == verified.Independent && prior.Extraction != verified.Extraction
}

func (s *Store) verifyPatternResolutionsTx(ctx context.Context, tx *sql.Tx, snap patternSnapshot, members []harness.SkillExperience) error {
	if snap.ResolutionPolicy == "" {
		if len(snap.Resolutions) != 0 {
			return invalid("unversioned pattern resolution")
		}
		return nil
	}
	expected, err := s.planPatternResolutionsTx(ctx, tx, snap, members)
	if err != nil {
		return err
	}
	if !slices.Equal(expected, snap.Resolutions) {
		return invalid("inconsistent saved pattern resolutions")
	}
	return nil
}

// Equivalent members retain their original outcomes. A canonical, source-only
// link can remove a historical blocker, never add another independent vote.
func (s *Store) planPatternResolutionsTx(ctx context.Context, tx *sql.Tx, snap patternSnapshot, members []harness.SkillExperience) ([]experienceResolution, error) {
	if snap.ResolutionPolicy != ResolutionPolicyVersion || len(members) != len(snap.Members) || len(members) > maxPatternMembers {
		return nil, invalid("invalid resolution plan")
	}
	captures := map[string]extractionSnapshot{}
	sourceTurns := 0
	capture := func(dep experienceDependency) (extractionSnapshot, error) {
		if saved, found := captures[dep.Extraction]; found {
			return saved, nil
		}
		fixed, err := s.resolutionCaptureTx(ctx, tx, dep)
		if err == nil {
			sourceTurns += len(fixed.Sources) + rootObservationCount(fixed)
			if sourceTurns > 32 {
				return fixed, invalid("resolution source-turn bounds")
			}
			captures[dep.Extraction] = fixed
		}
		return fixed, err
	}
	var result []experienceResolution
	for i, prior := range members {
		if prior.Candidate.Outcome == "observed_execution" {
			continue
		}
		var chosen string
		var accepted int64
		for j, verified := range members {
			if verified.Candidate.Outcome != "observed_execution" || !sameResolutionTask(snap.Members[i], snap.Members[j]) {
				continue
			}
			before, err := capture(snap.Members[i])
			if err != nil {
				return nil, err
			}
			after, err := capture(snap.Members[j])
			if err != nil {
				return nil, err
			}
			sequence := resolutionAccepted(before, after, prior.Candidate, verified.Candidate)
			if sequence > 0 && (chosen == "" || sequence < accepted || sequence == accepted && verified.ID < chosen) {
				chosen, accepted = verified.ID, sequence
			}
		}
		if chosen != "" {
			result = append(result, experienceResolution{Prior: prior.ID, Verified: chosen})
		}
	}
	return result, nil
}

// History reads check fixed capture identities, not live task trees. The live
// pattern path separately revalidates every original extraction and source.
func (s *Store) resolutionCaptureTx(ctx context.Context, tx *sql.Tx, dep experienceDependency) (extractionSnapshot, error) {
	var snap extractionSnapshot
	var data []byte
	var sha, resultSHA, policy, model, session, episode, jobID string
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha,result_sha,policy_version,model_identity,session_id,episode_id,epoch,job_id
 FROM df_learning_extractions WHERE id=?`, dep.Extraction).Scan(&data, &sha, &resultSHA, &policy, &model, &session, &episode, &epoch, &jobID); err != nil {
		return snap, err
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, jobID)
	if err != nil {
		return snap, err
	}
	if len(data) > (2<<20)+(32<<10) || digest(data) != sha || sha != dep.SourceSHA || resultSHA == "" || resultSHA != dep.ResultSHA || json.Unmarshal(data, &snap) != nil ||
		policy != PolicyVersion || model == "" || session != dep.Session || episode != dep.Episode || epoch != dep.Epoch || snap.ID != dep.Extraction || snap.Session != session || snap.Episode != episode || snap.Epoch != epoch || snap.View.EpisodeID != episode ||
		len(snap.Sources) < 1 || len(snap.Sources) > 8 || rootObservationCount(snap) > 8 || len(snap.View.References) > 528 || job.State != "completed" ||
		job.Request.Kind != skillmaintenance.ExperienceExtract || job.Request.OwnerKind != skillmaintenance.WorkspaceOwner || job.Request.OwnerKey != snap.Owner || job.Request.Key != snap.ID || job.Request.Reference != snap.ID || job.Request.InputDigest != sha || snap.ID != extractionID(snap, model) {
		return snap, invalid("inconsistent resolution extraction")
	}
	return snap, nil
}

func resolutionAccepted(before, after extractionSnapshot, prior, verified harness.SkillExperienceCandidate) int64 {
	if before.Session != after.Session || before.Episode != after.Episode || before.Epoch != after.Epoch || before.Owner != after.Owner || prior.Outcome == "observed_execution" || verified.Outcome != "observed_execution" {
		return 0
	}
	refs := map[string]harness.SkillLearningReference{}
	for _, ref := range after.View.References {
		refs[ref.ID] = ref
	}
	covered := false
	for _, ref := range before.View.References {
		if ref.Kind != "user" || !slices.Contains(prior.References, ref.ID) {
			continue
		}
		later, found := refs[ref.ID]
		if !found || ref.Truncated || later.Truncated || !slices.Contains(verified.References, ref.ID) || encodedDigest(ref) != encodedDigest(later) {
			return 0
		}
		covered = true
	}
	if !covered {
		return 0
	}
	var frontier, outbox int64
	for _, src := range before.Sources {
		frontier, outbox = max(frontier, src.Frontier), max(outbox, src.Outbox)
	}
	for _, root := range before.Roots {
		for _, member := range root.Members {
			frontier = max(frontier, member.Frontier)
		}
	}
	var accepted int64
	for _, src := range after.Sources {
		if src.Accepted <= frontier || src.Outbox <= outbox {
			continue
		}
		user, found := refs["input/"+src.Input]
		if !found || user.Kind != "user" || user.RunID != src.Run || user.InputID != src.Input || user.Sequence != src.Accepted || user.Truncated || !slices.Contains(verified.References, user.ID) {
			continue
		}
		for _, ref := range after.View.References {
			if ref.Kind != "tool_observation" || ref.State != string(harness.ReceiptCompleted) || ref.Truncated || !localObservation(ref.ToolName) || !slices.Contains(verified.References, ref.ID) {
				continue
			}
			if _, completed := observationEnded(after, src, ref.RunID); completed && (accepted == 0 || src.Accepted < accepted) {
				accepted = src.Accepted
			}
		}
	}
	return accepted
}
