package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func (s *Store) readExtractionTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) (snap extractionSnapshot, result string, err error) {
	ctx, finish := withSourceActivity(ctx, tx)
	defer finish(&err)
	cache := transactionEvidenceCache(ctx, tx)
	if cache != nil {
		if saved, found := cache.extractions[job.ID]; found && saved.request == job.Request {
			if saved.active {
				_ = sourceActiveTx(ctx, tx)
			}
			return saved.snapshot, saved.result, ctx.Err()
		}
	}
	var data []byte
	var sha, policy, model string
	var session, episode string
	var epoch int64
	err = tx.QueryRowContext(ctx, `SELECT payload,payload_sha,policy_version,model_identity,result_sha,session_id,episode_id,epoch FROM df_learning_extractions WHERE job_id=? AND id=?`, job.ID, job.Request.Reference).Scan(&data, &sha, &policy, &model, &result, &session, &episode, &epoch)
	if err != nil {
		return snap, result, err
	}
	if len(data) > s.inputLimit()+(32<<10) || digest(data) != sha || sha != job.Request.InputDigest || json.Unmarshal(data, &snap) != nil || snap.ID != job.Request.Reference || job.Request.Key != snap.ID || snap.Session != session || snap.Episode != episode || snap.Epoch != epoch || snap.MaxDailyModelCalls < 1 || snap.MaxDailyTokens < 1 || job.Request.Kind != skillmaintenance.ExperienceExtract || job.Request.OwnerKind != skillmaintenance.WorkspaceOwner || job.Request.OwnerKey != snap.Owner {
		return snap, result, invalid("inconsistent extraction identity")
	}
	if s.extractor == nil || policy != PolicyVersion || model != s.extractor.Identity() {
		return snap, result, harness.ErrSkillConflict
	}
	if result != "" {
		if err = s.verifyResultTx(ctx, tx, snap, result); err != nil {
			return snap, result, err
		}
	}
	if cache != nil && !cache.admitSourceTurns(len(snap.Sources)+rootObservationCount(snap)) {
		return snap, result, invalid("consolidation source-turn bounds")
	}
	if err = s.verifySnapshotTx(ctx, tx, snap); err != nil {
		return snap, result, err
	}
	if snap.ID != extractionID(snap, model) {
		return snap, result, invalid("inconsistent extraction snapshot identity")
	}
	if cache != nil {
		cache.extractions[job.ID] = verifiedExtraction{request: job.Request, snapshot: snap, result: result, active: sourceActivityRecorded(ctx, tx)}
	}
	return snap, result, nil
}

// A crash after the result commit but before job ACK must reuse the committed
// output, not call the model again or acknowledge missing/corrupted records.
func (s *Store) verifyResultTx(ctx context.Context, tx *sql.Tx, snap extractionSnapshot, resultSHA string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,payload,payload_sha,session_id,episode_id,revision FROM df_learning_experiences WHERE extraction_id=? LIMIT 5`, snap.ID)
	if err != nil {
		return err
	}
	var saved []harness.SkillExperience
	for rows.Next() {
		var id, sha, session, episode string
		var revision int64
		var data []byte
		var e harness.SkillExperience
		if err = rows.Scan(&id, &data, &sha, &session, &episode, &revision); err != nil {
			break
		}
		if len(data) > 64<<10 || digest(data) != sha || json.Unmarshal(data, &e) != nil || e.ID != id || session != snap.Session || episode != snap.Episode || e.SessionID != session || e.EpisodeID != episode || e.Revision != revision || revision != 1 || e.Origin != "model_extracted" || e.Quality != "unassessed" {
			err = invalid("inconsistent saved experience")
			break
		}
		saved = append(saved, e)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(saved) > 4 {
		return invalid("too many saved experiences")
	}
	candidates := make([]harness.SkillExperienceCandidate, len(saved))
	seen := make([]bool, len(saved))
	for _, e := range saved {
		found := false
		for i := range candidates {
			if e.ID == "lexp_"+encodedDigest([]any{snap.ID, i, e.Candidate}) && !seen[i] {
				candidates[i], seen[i], found = e.Candidate, true, true
				break
			}
		}
		if !found {
			return invalid("inconsistent saved experience order")
		}
	}
	validated, err := validateCandidates(snap.View, candidates)
	if err != nil || encodedDigest(candidates) != resultSHA || encodedDigest(validated) != resultSHA {
		return invalid("inconsistent saved extraction result")
	}
	return nil
}

func (s *Store) AdmitTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return harness.ErrPermissionDenied
	}
	switch job.Request.Kind {
	case skillmaintenance.ExperienceExtract:
		_, _, err := s.readExtractionTx(ctx, tx, job)
		return err
	case skillmaintenance.ExperienceConsolidate:
		_, _, err := s.readConsolidationTx(ctx, tx, job)
		return err
	case skillmaintenance.SkillMatch:
		_, _, err := s.readMatchTx(ctx, tx, job)
		return err
	default:
		return invalid("unsupported learning producer")
	}
}

func (s *Store) AdmitCallTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job, call skillmaintenance.ModelCall, now time.Time) error {
	maxCalls, maxTokens := s.options.MaxDailyModelCalls, s.options.MaxDailyTokens
	if s.extractor != nil {
		switch job.Request.Kind {
		case skillmaintenance.ExperienceExtract:
			snap, _, err := s.readExtractionTx(ctx, tx, job)
			if err != nil {
				return err
			}
			maxCalls, maxTokens = min(maxCalls, snap.MaxDailyModelCalls), min(maxTokens, snap.MaxDailyTokens)
		case skillmaintenance.ExperienceConsolidate:
			snap, _, err := s.readConsolidationTx(ctx, tx, job)
			if err != nil {
				return err
			}
			maxCalls, maxTokens = min(maxCalls, snap.MaxDailyModelCalls), min(maxTokens, snap.MaxDailyTokens)
		case skillmaintenance.SkillMatch:
			snap, _, err := s.readMatchTx(ctx, tx, job)
			if err != nil {
				return err
			}
			maxCalls, maxTokens = min(maxCalls, snap.MaxDailyModelCalls), min(maxTokens, snap.MaxDailyTokens)
		default:
			return invalid("unsupported learning call")
		}
	}
	var calls, tokens int64
	err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(c.charged_tokens),0) FROM df_skill_maintenance_calls c
 JOIN df_skill_maintenance_jobs j ON j.id=c.job_id WHERE c.day=? AND j.job_kind IN ('experience_extract','experience_consolidate','skill_match')`, now.UTC().Format("2006-01-02")).Scan(&calls, &tokens)
	if err != nil {
		return err
	}
	if calls >= maxCalls || tokens > maxTokens-call.InputTokens-call.MaxOutputTokens {
		return skillmaintenance.ErrBudget
	}
	return nil
}

func (s *Store) FinalizeTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	if job.Request.Kind == skillmaintenance.SkillMatch {
		return s.FinalizeMatchTx(ctx, tx, job)
	}
	if job.Request.Kind == skillmaintenance.ExperienceConsolidate {
		return s.FinalizeConsolidationTx(ctx, tx, job)
	}
	if job.Request.Kind != skillmaintenance.ExperienceExtract {
		return invalid("unsupported learning terminal")
	}
	state := "processed"
	if job.State != "completed" {
		state = "skipped"
	}
	_, err := tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state=?,reason=? WHERE job_id=? AND state='queued'`, state, job.ErrorCode, job.ID)
	return err
}

func learningFailure(err error) (string, bool) {
	switch {
	case errors.Is(err, skillmaintenance.ErrMatchInventoryChanged):
		return "inventory_changed", false
	case errors.Is(err, skillmaintenance.ErrSourceActive):
		return "source_active", true
	case errors.Is(err, harness.ErrPermissionDenied), errors.Is(err, sql.ErrNoRows):
		return "evidence_changed", false
	case errors.Is(err, harness.ErrSkillConflict):
		return "configuration_changed", false
	case errors.Is(err, skillmaintenance.ErrBudget):
		return "budget_exhausted", false
	case errors.Is(err, harness.ErrInvalidInput):
		return "invalid_candidate", false
	case errors.Is(err, harness.ErrSkillValidation):
		return "model_failed", true
	default:
		return "storage_error", true
	}
}

func (s *Store) Handle(ctx context.Context, job skillmaintenance.Job) (string, bool) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return learningFailure(err)
	}
	snap, result, err := s.readExtractionTx(ctx, tx, job)
	if err == nil {
		err = skillmaintenance.GuardTx(ctx, tx, job.ID)
	}
	_ = tx.Rollback()
	if err != nil {
		return learningFailure(err)
	}
	if result != "" {
		return "", false
	}
	if err = ctx.Err(); err != nil {
		return "cancelled", true
	}
	// Give adapters their own copy. Even a trusted custom adapter must not
	// mutate the host's reference inventory used to validate returned IDs.
	data, _ := json.Marshal(snap.View)
	var view harness.SkillLearningInput
	if err = json.Unmarshal(data, &view); err != nil {
		return learningFailure(err)
	}
	candidates, err := s.extractor.Extract(ctx, view)
	if err != nil {
		return learningFailure(err)
	}
	candidates, err = validateCandidates(snap.View, candidates)
	if err != nil {
		return learningFailure(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return learningFailure(err)
	}
	defer tx.Rollback()
	if err = skillmaintenance.GuardTx(ctx, tx, job.ID); err != nil {
		return learningFailure(err)
	}
	if _, result, err = s.readExtractionTx(ctx, tx, job); err != nil || result != "" {
		if err == nil {
			err = harness.ErrSkillConflict
		}
		return learningFailure(err)
	}
	for i, candidate := range candidates {
		e := harness.SkillExperience{ID: "lexp_" + encodedDigest([]any{snap.ID, i, candidate}), SessionID: snap.Session, EpisodeID: snap.Episode, Revision: 1, Origin: "model_extracted", Quality: "unassessed", Candidate: candidate, CreatedAt: s.now().UTC()}
		data, _ := json.Marshal(e)
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_experiences VALUES(?,?,?,?,?,?,?,?)`, e.ID, snap.ID, snap.Session, snap.Episode, e.Revision, data, digest(data), e.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return learningFailure(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_consolidation_inputs(experience_id) VALUES(?)`, e.ID); err != nil {
			return learningFailure(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_extractions SET result_sha=? WHERE id=? AND result_sha=''`, encodedDigest(candidates), snap.ID); err != nil {
		return learningFailure(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state='processed',reason='' WHERE job_id=? AND state='queued'`, job.ID); err != nil {
		return learningFailure(err)
	}
	if err = tx.Commit(); err != nil {
		return learningFailure(err)
	}
	return "", false
}

func validText(text string, limit int, required bool) bool {
	return (!required || strings.TrimSpace(text) != "") && len(text) <= limit && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

func validateCandidates(input harness.SkillLearningInput, candidates []harness.SkillExperienceCandidate) ([]harness.SkillExperienceCandidate, error) {
	if len(candidates) > 4 {
		return nil, invalid("too many experiences")
	}
	refs := map[string]harness.SkillLearningReference{}
	for _, ref := range input.References {
		refs[ref.ID] = ref
	}
	result := make([]harness.SkillExperienceCandidate, 0, len(candidates))
	seen := map[string]bool{}
	for _, c := range candidates {
		switch c.Kind {
		case "correction_chain", "proven_recipe", "capability_gap", "trigger_mismatch":
		default:
			return nil, invalid("unsupported experience kind")
		}
		if !validText(c.Goal, 1000, true) || !validText(c.Applicability, 2000, true) || !validText(c.Problem, 2000, c.Kind == "correction_chain") || !validText(c.Resolution, 4000, true) || !validText(c.Verification, 2000, true) || len(c.Steps) < 1 || len(c.Steps) > 12 || len(c.References) < 1 || len(c.References) > 32 {
			return nil, invalid("experience exceeds text bounds")
		}
		var user, observed, skill bool
		used := map[string]bool{}
		for _, id := range c.References {
			ref, exists := refs[id]
			if !exists || used[id] {
				return nil, invalid("invented or duplicate experience reference")
			}
			used[id] = true
			user = user || ref.Kind == "user"
			skill = skill || ref.Kind == "skill_read"
			observed = observed || ref.Kind == "tool_observation" && ref.State == string(harness.ReceiptCompleted) && !ref.Truncated && localObservation(ref.ToolName)
		}
		if !user || c.Kind == "trigger_mismatch" && !skill || c.Outcome != "observed_execution" && c.Outcome != "user_reported" && c.Outcome != "unresolved" || c.Outcome == "observed_execution" && !observed || c.Kind == "proven_recipe" && c.Outcome != "observed_execution" {
			return nil, invalid("unsupported experience outcome provenance")
		}
		c.Goal, c.Applicability, c.Problem = redact(c.Goal, 1000), redact(c.Applicability, 2000), redact(c.Problem, 2000)
		c.Resolution, c.Verification = redact(c.Resolution, 4000), redact(c.Verification, 2000)
		for i, step := range c.Steps {
			if !validText(step, 1000, true) {
				return nil, invalid("invalid experience step")
			}
			c.Steps[i] = redact(step, 1000)
		}
		key := encodedDigest(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, c)
	}
	return result, nil
}
