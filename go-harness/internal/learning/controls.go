package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func (s *Store) SetControl(ctx context.Context, sessionID string, request harness.SkillLearningControlRequest) (harness.SkillLearningControl, error) {
	result := harness.SkillLearningControl{SessionID: sessionID, Contribute: request.Contribute}
	if request.ExpectedEpoch < 0 {
		return result, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_sessions WHERE id=?)`, sessionID).Scan(&exists); err != nil {
		return result, err
	}
	if !exists {
		return result, harness.ErrNotFound
	}
	var current int64
	var prior, shared bool
	err = tx.QueryRowContext(ctx, `SELECT c.epoch,c.contribute,COALESCE(sh.workspace_shared,0)
 FROM harness_learning_controls c LEFT JOIN harness_learning_sharing sh ON sh.session_id=c.session_id WHERE c.session_id=?`, sessionID).Scan(&current, &prior, &shared)
	if errors.Is(err, sql.ErrNoRows) {
		current, err = 0, nil
	}
	if err != nil {
		return result, err
	}
	if current != request.ExpectedEpoch {
		return result, harness.ErrSkillConflict
	}
	result.WorkspaceShared = shared
	if request.WorkspaceShared != nil {
		result.WorkspaceShared = *request.WorkspaceShared
	}
	if !request.Contribute {
		if request.WorkspaceShared != nil && *request.WorkspaceShared {
			return result, harness.ErrInvalidInput
		}
		result.WorkspaceShared = false
	}
	if result.WorkspaceShared && request.WorkspaceShared != nil && (!s.enabled || !s.options.CrossSession || s.options.Consolidation == nil) {
		return result, harness.ErrPermissionDenied
	}
	if current > 0 && prior == request.Contribute && shared == result.WorkspaceShared {
		result.Epoch = current
		return result, tx.Commit()
	}
	result.Epoch = current + 1
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_learning_controls VALUES(?,?,?) ON CONFLICT(session_id) DO UPDATE SET contribute=excluded.contribute,epoch=excluded.epoch`, sessionID, result.Contribute, result.Epoch); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_learning_sharing VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET workspace_shared=excluded.workspace_shared`, sessionID, result.WorkspaceShared); err != nil {
		return result, err
	}
	if err = ForgetSessionTx(ctx, tx, sessionID); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// ForgetSessionTx deletes unpublished derived text and revokes active claims in
// the same transaction. Accounting survives; no stale worker can write it back.
func ForgetSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var jobs bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_maintenance_jobs')`).Scan(&jobs); err != nil {
		return err
	}
	if jobs {
		if _, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE id IN (SELECT job_id FROM df_learning_extractions WHERE session_id=?
 UNION SELECT job_id FROM df_learning_consolidations WHERE session_id=?
 UNION SELECT job_id FROM df_learning_matches WHERE session_id=?) AND state IN ('queued','retry','running')`, sessionID, sessionID, sessionID); err != nil {
			return err
		}
	}
	if err := forgetDerivedTx(ctx, tx, sessionID, jobs); err != nil {
		return err
	}
	for _, query := range []string{
		`DELETE FROM df_learning_matches WHERE session_id=?`,
		`DELETE FROM df_learning_patterns WHERE session_id=?`,
		`DELETE FROM df_learning_consolidations WHERE session_id=?`,
		`DELETE FROM df_learning_experiences WHERE session_id=?`,
		`DELETE FROM df_learning_extractions WHERE session_id=?`,
		`DELETE FROM harness_learning_outbox WHERE session_id=?`,
		`DELETE FROM harness_learning_inputs WHERE session_id=?`,
		`DELETE FROM df_learning_episodes WHERE session_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, query, sessionID); err != nil {
			return err
		}
	}
	return nil
}

// StartEpisode is an explicit host/user task boundary. Extraction cannot split
// one conversation into several independent votes just by proposing labels.
func (s *Store) StartEpisode(ctx context.Context, sessionID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var workspace string
	if err = tx.QueryRowContext(ctx, `SELECT cwd FROM harness_sessions WHERE id=?`, sessionID).Scan(&workspace); err != nil {
		return "", err
	}
	id, err := insertEpisodeTx(ctx, tx, sessionID, workspaceKey(workspace), "explicit_task", "")
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) Experiences(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperiencePage, error) {
	page := harness.SkillExperiencePage{Experiences: []harness.SkillExperience{}}
	if len(after) > 128 || limit < 1 || limit > 100 {
		return page, harness.ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,payload,payload_sha FROM df_learning_experiences WHERE session_id=? AND id>? ORDER BY id LIMIT ?`, sessionID, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sha string
		var data []byte
		if err = rows.Scan(&id, &data, &sha); err != nil {
			return page, err
		}
		if len(page.Experiences) == limit {
			page.Next = page.Experiences[len(page.Experiences)-1].ID
			break
		}
		var e harness.SkillExperience
		if len(data) > 64<<10 || digest(data) != sha || json.Unmarshal(data, &e) != nil || e.ID != id || e.SessionID != sessionID || e.Origin != "model_extracted" || e.Quality != "unassessed" {
			return page, invalid("inconsistent experience")
		}
		page.Experiences = append(page.Experiences, e)
	}
	return page, rows.Err()
}

func (s *Store) CancelExtraction(ctx context.Context, sessionID, jobID string, version int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var belongs bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_extractions WHERE session_id=? AND job_id=?
 UNION SELECT 1 FROM df_learning_consolidations WHERE session_id=? AND job_id=?
 UNION SELECT 1 FROM df_learning_matches WHERE session_id=? AND job_id=?)`, sessionID, jobID, sessionID, jobID, sessionID, jobID).Scan(&belongs); err != nil {
		return err
	}
	if !belongs {
		return harness.ErrNotFound
	}
	if err = skillmaintenance.CancelTx(ctx, tx, jobID, version); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state='cancelled',reason='cancelled' WHERE session_id=? AND job_id=?`, sessionID, jobID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET state='cancelled',reason='cancelled'
 WHERE job_id=? AND experience_id IN (SELECT id FROM df_learning_experiences WHERE session_id=?)`, jobID, sessionID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET state='cancelled',reason='cancelled'
 WHERE job_id=? AND pattern_id IN (SELECT id FROM df_learning_patterns WHERE session_id=?)`, jobID, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Jobs(ctx context.Context, sessionID, after string, limit int) (harness.SkillLearningJobPage, error) {
	page := harness.SkillLearningJobPage{Jobs: []harness.SkillLearningJob{}}
	if len(after) > 128 || limit < 1 || limit > 100 {
		return page, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_maintenance_jobs')`).Scan(&exists); err != nil || !exists {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT job_id,episode_id,pattern_id,pattern_revision FROM (
	 SELECT job_id,episode_id,'' AS pattern_id,0 AS pattern_revision FROM df_learning_extractions WHERE session_id=?
	 UNION ALL SELECT c.job_id,e.episode_id,'',0 FROM df_learning_consolidations c JOIN df_learning_experiences e ON e.id=c.experience_id WHERE c.session_id=?
	 UNION ALL SELECT m.job_id,'',m.pattern_id,m.revision FROM df_learning_matches m WHERE m.session_id=?
 AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_sharing sh WHERE sh.pattern_id=m.pattern_id))
 WHERE job_id>? ORDER BY job_id LIMIT ?`, sessionID, sessionID, sessionID, after, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var j harness.SkillLearningJob
		if err = rows.Scan(&j.ID, &j.EpisodeID, &j.PatternID, &j.PatternRevision); err != nil {
			break
		}
		if len(page.Jobs) == limit {
			page.Next = page.Jobs[len(page.Jobs)-1].ID
			break
		}
		page.Jobs = append(page.Jobs, j)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return page, err
	}
	for i := range page.Jobs {
		j, err := skillmaintenance.ReadJob(ctx, tx, page.Jobs[i].ID)
		if err != nil {
			return page, err
		}
		if j.Request.Kind != skillmaintenance.ExperienceExtract && j.Request.Kind != skillmaintenance.ExperienceConsolidate && j.Request.Kind != skillmaintenance.SkillMatch {
			return page, invalid("inconsistent learning job")
		}
		page.Jobs[i].Kind, page.Jobs[i].State, page.Jobs[i].StateVersion = string(j.Request.Kind), j.State, j.StateVersion
		page.Jobs[i].Attempts, page.Jobs[i].ErrorCode, page.Jobs[i].Deadline = j.Attempts, j.ErrorCode, j.Deadline
	}
	return page, tx.Commit()
}
