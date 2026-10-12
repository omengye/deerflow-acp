package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type consolidationResult struct {
	Decision harness.SkillExperienceConsolidationDecision `json:"decision"`
	Patterns []patternSnapshot                            `json:"patterns"`
	At       time.Time                                    `json:"created_at"`
}

func validateConsolidationDecision(input harness.SkillExperienceConsolidationInput, decision harness.SkillExperienceConsolidationDecision) (harness.SkillExperienceConsolidationDecision, error) {
	if !validText(decision.Reason, 2000, true) {
		return decision, invalid("invalid consolidation explanation")
	}
	switch decision.Relation {
	case "distinct", "ambiguous":
		if decision.PatternID != "" {
			return decision, invalid("untargeted consolidation has a pattern")
		}
	case "equivalent", "conflicting":
		found := false
		for _, choice := range input.Patterns {
			found = found || choice.ID == decision.PatternID
		}
		if !found {
			return decision, invalid("invented consolidation target")
		}
	default:
		return decision, invalid("unsupported consolidation relation")
	}
	decision.Reason = redact(decision.Reason, 2000)
	if !validText(decision.Reason, 2000, true) {
		return decision, invalid("empty redacted consolidation explanation")
	}
	return decision, nil
}

func (s *Store) readConsolidationTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) (out consolidationSnapshot, saved *consolidationResult, err error) {
	ctx, finish := withSourceActivity(ctx, tx)
	defer finish(&err)
	ctx = withEvidenceCache(ctx, tx)
	var snap consolidationSnapshot
	var data, output []byte
	var sha, policy, model, resultSHA, session, seed string
	err = tx.QueryRowContext(ctx, `SELECT payload,payload_sha,policy_version,model_identity,result,result_sha,session_id,experience_id
 FROM df_learning_consolidations WHERE job_id=? AND id=?`, job.ID, job.Request.Reference).Scan(&data, &sha, &policy, &model, &output, &resultSHA, &session, &seed)
	if err != nil {
		return snap, nil, err
	}
	if len(data) > s.consolidationInputLimit()+(32<<10) || digest(data) != sha || sha != job.Request.InputDigest || json.Unmarshal(data, &snap) != nil || snap.ID != job.Request.Reference || job.Request.Key != snap.ID || snap.Session != session || snap.Seed.ID != seed || snap.Seed.Session != session || job.Request.Kind != skillmaintenance.ExperienceConsolidate || job.Request.OwnerKind != skillmaintenance.WorkspaceOwner || job.Request.OwnerKey != snap.Owner || snap.MaxDailyModelCalls < 1 || snap.MaxDailyTokens < 1 || len(snap.Patterns) > 8 || len(snap.Patterns) != len(snap.View.Patterns) {
		return snap, nil, invalid("inconsistent consolidation identity")
	}
	if s.consolidator == nil || s.options.Consolidation == nil || policy != ConsolidationPolicyVersion || model != s.consolidator.Identity() {
		return snap, nil, harness.ErrSkillConflict
	}
	identity := snap
	identity.ID = ""
	if snap.ResolutionPolicy != "" && snap.ResolutionPolicy != ResolutionPolicyVersion || snap.ID != "lcon_"+encodedDigest([]any{policy, model, identity}) {
		return snap, nil, invalid("inconsistent consolidation policy digest")
	}
	e, live, err := s.experienceTx(ctx, tx, snap.Seed.ID)
	if err != nil {
		return snap, nil, err
	}
	if live != snap.Seed || encodedDigest(snap.View.Seed) != encodedDigest(harness.SkillExperienceConsolidationSeed{ID: e.ID, EpisodeID: e.EpisodeID, Candidate: e.Candidate}) {
		return snap, nil, harness.ErrPermissionDenied
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT workspace_key FROM df_learning_episodes WHERE id=?`, live.Episode).Scan(&owner); err != nil || owner != snap.Owner {
		if err == nil {
			err = harness.ErrPermissionDenied
		}
		return snap, nil, err
	}
	shared, err := s.seedSharedTx(ctx, tx, live, owner)
	if err != nil || shared != snap.Shared {
		if err == nil {
			err = harness.ErrPermissionDenied
		}
		return snap, nil, err
	}
	seen := map[string]bool{}
	for i, dep := range snap.Patterns {
		if seen[dep.ID] || dep.Revision < 1 {
			return snap, nil, invalid("duplicate consolidation pattern")
		}
		seen[dep.ID] = true
		p, err := s.patternTx(ctx, tx, dep.ID, dep.Revision, resultSHA == "")
		if err != nil {
			return snap, nil, err
		}
		choice := harness.SkillExperienceConsolidationChoice{ID: p.Pattern.ID, Revision: p.Pattern.Revision, Candidate: p.Pattern.Candidate, EvidenceState: p.Pattern.EvidenceState}
		if p.Shared != snap.Shared || !snap.Shared && p.Pattern.SessionID != session || encodedDigest(p) != dep.SHA || encodedDigest(choice) != encodedDigest(snap.View.Patterns[i]) {
			return snap, nil, harness.ErrPermissionDenied
		}
	}
	if snap.Shared {
		if err = verifyConsolidationSourcesTx(ctx, tx, job.ID, snap); err != nil {
			return snap, nil, err
		}
	}
	if resultSHA == "" {
		if len(output) != 0 {
			return snap, nil, invalid("uncommitted consolidation result")
		}
		var queued bool
		if err = tx.QueryRowContext(ctx, `SELECT state='queued' AND job_id=? FROM df_learning_consolidation_inputs WHERE experience_id=?`, job.ID, seed).Scan(&queued); err != nil || !queued {
			if err == nil {
				err = harness.ErrPermissionDenied
			}
			return snap, nil, err
		}
		return snap, nil, nil
	}
	var result consolidationResult
	if len(output) > 2*patternStorageLimit+(8<<10) || digest(output) != resultSHA || json.Unmarshal(output, &result) != nil || result.At.IsZero() {
		return snap, nil, invalid("inconsistent consolidation result")
	}
	decision, err := validateConsolidationDecision(snap.View, result.Decision)
	if err != nil || encodedDigest(decision) != encodedDigest(result.Decision) {
		return snap, nil, invalid("inconsistent saved consolidation decision")
	}
	expected, err := s.planConsolidationTx(ctx, tx, snap, decision, result.At)
	if err != nil {
		return snap, nil, err
	}
	if encodedDigest(expected) != encodedDigest(result.Patterns) {
		return snap, nil, invalid("inconsistent saved consolidation plan")
	}
	for _, expected := range result.Patterns {
		saved, err := s.patternTx(ctx, tx, expected.Pattern.ID, expected.Pattern.Revision, false)
		if err != nil {
			return snap, nil, err
		}
		if encodedDigest(saved) != encodedDigest(expected) {
			return snap, nil, invalid("missing or changed consolidation output")
		}
	}
	var processed bool
	if err = tx.QueryRowContext(ctx, `SELECT state='processed' AND job_id=? FROM df_learning_consolidation_inputs WHERE experience_id=?`, job.ID, seed).Scan(&processed); err != nil || !processed {
		if err == nil {
			err = invalid("missing consolidation acknowledgment")
		}
		return snap, nil, err
	}
	return snap, &result, nil
}

func (s *Store) planConsolidationTx(ctx context.Context, tx *sql.Tx, snap consolidationSnapshot, decision harness.SkillExperienceConsolidationDecision, now time.Time) ([]patternSnapshot, error) {
	seed, dep, err := s.experienceTx(ctx, tx, snap.Seed.ID)
	if err != nil {
		return nil, err
	}
	var target patternSnapshot
	if decision.PatternID != "" {
		for _, ref := range snap.Patterns {
			if ref.ID == decision.PatternID {
				target, err = s.patternTx(ctx, tx, ref.ID, ref.Revision, false)
				break
			}
		}
		if err != nil || target.Pattern.ID == "" {
			if err == nil {
				err = invalid("missing consolidation target")
			}
			return nil, err
		}
	}
	if decision.Relation == "equivalent" {
		if len(target.Members) >= maxPatternMembers || slices.Contains(target.Pattern.ExperienceIDs, dep.ID) {
			return nil, invalid("pattern member capacity or duplicate")
		}
		target.Members = append(target.Members, dep)
		sort.Slice(target.Members, func(i, j int) bool { return target.Members[i].ID < target.Members[j].ID })
		members, err := s.patternExperiencesTx(ctx, tx, target.Members)
		if err != nil {
			return nil, err
		}
		representative := target.Pattern.RepresentativeID
		if target.Pattern.Candidate.Outcome != "observed_execution" && seed.Candidate.Outcome == "observed_execution" {
			representative = seed.ID
		}
		if snap.ResolutionPolicy != "" {
			target.ResolutionPolicy = snap.ResolutionPolicy
			target.Resolutions, err = s.planPatternResolutionsTx(ctx, tx, target, members)
			if err != nil {
				return nil, err
			}
		}
		target.Pattern.Revision++
		target.Pattern.RepresentativeID, target.Pattern.UpdatedAt = representative, now
		target.Pattern, err = projectPattern(target, members)
		return []patternSnapshot{target}, err
	}
	newID := "lpat_" + encodedDigest([]string{ConsolidationPolicyVersion, snap.Session, seed.ID})
	conflicts := []string{}
	if decision.Relation == "conflicting" {
		conflicts = append(conflicts, target.Pattern.ID)
	}
	p, err := buildPattern(newID, snap.Session, 1, seed.ID, []experienceDependency{dep}, []harness.SkillExperience{seed}, conflicts, decision.Relation == "ambiguous", now)
	if err != nil {
		return nil, err
	}
	outputs := []patternSnapshot{{Pattern: p, Members: []experienceDependency{dep}, Shared: snap.Shared, ResolutionPolicy: snap.ResolutionPolicy}}
	if decision.Relation == "conflicting" {
		members, err := s.patternExperiencesTx(ctx, tx, target.Members)
		if err != nil {
			return nil, err
		}
		conflicts = append(slices.Clone(target.Pattern.ConflictsWith), newID)
		target.Pattern.Revision++
		target.Pattern.ConflictsWith, target.Pattern.UpdatedAt = conflicts, now
		target.Pattern, err = projectPattern(target, members)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, target)
	}
	return outputs, nil
}

func (s *Store) patternExperiencesTx(ctx context.Context, tx *sql.Tx, deps []experienceDependency) ([]harness.SkillExperience, error) {
	ctx = withEvidenceCache(ctx, tx)
	result := make([]harness.SkillExperience, 0, len(deps))
	for _, dep := range deps {
		e, live, err := s.experienceTx(ctx, tx, dep.ID)
		if err != nil {
			return nil, err
		}
		if dep != live {
			return nil, harness.ErrPermissionDenied
		}
		result = append(result, e)
	}
	return result, nil
}

func (s *Store) HandleConsolidation(ctx context.Context, job skillmaintenance.Job) (string, bool) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return learningFailure(err)
	}
	ctx = withEvidenceCache(ctx, tx)
	snap, saved, err := s.readConsolidationTx(ctx, tx, job)
	if err == nil {
		err = skillmaintenance.GuardTx(ctx, tx, job.ID)
	}
	_ = tx.Rollback()
	if err != nil {
		return learningFailure(err)
	}
	if saved != nil {
		return "", false
	}
	data, _ := json.Marshal(snap.View)
	var view harness.SkillExperienceConsolidationInput
	if err = json.Unmarshal(data, &view); err != nil {
		return learningFailure(err)
	}
	decision, err := s.consolidator.Consolidate(ctx, view)
	if err != nil {
		return learningFailure(err)
	}
	decision, err = validateConsolidationDecision(snap.View, decision)
	if err != nil {
		return learningFailure(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return learningFailure(err)
	}
	defer tx.Rollback()
	ctx = withEvidenceCache(ctx, tx)
	if err = skillmaintenance.GuardTx(ctx, tx, job.ID); err != nil {
		return learningFailure(err)
	}
	if _, saved, err = s.readConsolidationTx(ctx, tx, job); err != nil || saved != nil {
		if err == nil {
			err = harness.ErrSkillConflict
		}
		return learningFailure(err)
	}
	now := s.now().UTC()
	outputs, err := s.planConsolidationTx(ctx, tx, snap, decision, now)
	if err != nil {
		return learningFailure(err)
	}
	for _, output := range outputs {
		if err = savePatternTx(ctx, tx, snap, output); err != nil {
			return learningFailure(err)
		}
	}
	data, _ = json.Marshal(consolidationResult{Decision: decision, Patterns: outputs, At: now})
	if len(data) > 2*patternStorageLimit+(8<<10) {
		return learningFailure(invalid("consolidation result exceeds bounds"))
	}
	result, err := tx.ExecContext(ctx, `UPDATE df_learning_consolidations SET result=?,result_sha=? WHERE id=? AND result_sha=''`, data, digest(data), snap.ID)
	if err != nil {
		return learningFailure(err)
	}
	if err = matchWriteApplied(result); err != nil {
		return learningFailure(err)
	}
	result, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET state='processed',reason='' WHERE experience_id=? AND state='queued' AND job_id=?`, snap.Seed.ID, job.ID)
	if err != nil {
		return learningFailure(err)
	}
	if err = matchWriteApplied(result); err != nil {
		return learningFailure(err)
	}
	if err = tx.Commit(); err != nil {
		return learningFailure(err)
	}
	return "", false
}

func savePatternTx(ctx context.Context, tx *sql.Tx, snap consolidationSnapshot, output patternSnapshot) error {
	p := output.Pattern
	if output.Shared != snap.Shared {
		return harness.ErrPermissionDenied
	}
	data, _ := json.Marshal(output)
	if len(data) > patternStorageLimit {
		return invalid("pattern revision exceeds storage bounds")
	}
	if p.Revision == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_patterns VALUES(?,?,?,?,?)`, p.ID, p.SessionID, snap.Owner, p.Revision, p.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if output.Shared {
			if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_pattern_sharing VALUES(?,?)`, p.ID, snap.Owner); err != nil {
				return err
			}
		}
	} else {
		result, err := tx.ExecContext(ctx, `UPDATE df_learning_patterns SET current_revision=?,updated_at=? WHERE id=? AND session_id=? AND owner_key=? AND current_revision=?`, p.Revision, p.UpdatedAt.Format(time.RFC3339Nano), p.ID, p.SessionID, snap.Owner, p.Revision-1)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return harness.ErrSkillConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_pattern_revisions VALUES(?,?,?,?,?)`, p.ID, p.Revision, snap.ID, data, digest(data)); err != nil {
		return err
	}
	for _, dep := range output.Members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_pattern_members VALUES(?,?,?,?,?,?,?)`, p.ID, p.Revision, dep.ID, dep.SHA, dep.Episode, dep.Independent, dep.Epoch); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_match_inputs(pattern_id,revision) VALUES(?,?)`, p.ID, p.Revision); err != nil {
		return err
	}
	return nil
}

func (s *Store) FinalizeConsolidationTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	state := "processed"
	if job.State != "completed" {
		state = "skipped"
	}
	_, err := tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET state=?,reason=? WHERE job_id=? AND state='queued'`, state, job.ErrorCode, job.ID)
	return err
}

func (s *Store) ConsolidationHooks() skillmaintenance.Hooks {
	return s.ExtractionHooks()
}

func (s *Store) handleLearning(ctx context.Context, job skillmaintenance.Job) (string, bool) {
	switch job.Request.Kind {
	case skillmaintenance.ExperienceExtract:
		return s.Handle(ctx, job)
	case skillmaintenance.ExperienceConsolidate:
		return s.HandleConsolidation(ctx, job)
	case skillmaintenance.SkillMatch:
		return s.HandleMatch(ctx, job)
	default:
		return "invalid_candidate", false
	}
}

func (s *Store) learningKinds() []skillmaintenance.JobKind {
	kinds := []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract}
	if s.consolidator != nil {
		kinds = append(kinds, skillmaintenance.ExperienceConsolidate)
	}
	if s.matcher != nil {
		kinds = append(kinds, skillmaintenance.SkillMatch)
	}
	return kinds
}
