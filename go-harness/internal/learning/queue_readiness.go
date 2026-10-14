package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

// Readiness defers only captured episodes, never an unrelated later task in
// the same session. Missing or invalid metadata goes to the normal admission
// guard; this scheduling hint cannot authorize model work or publication.
func (s *Store) ReadyTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) (bool, error) {
	if ready, err := s.budgetReadyTx(ctx, tx, job); err != nil || !ready {
		return ready, err
	}
	var episodes string
	var args []any
	switch job.Request.Kind {
	case skillmaintenance.ExperienceExtract:
		episodes = `SELECT episode_id FROM df_learning_extractions WHERE id=? AND job_id=?`
		args = []any{job.Request.Reference, job.ID}
	case skillmaintenance.ExperienceConsolidate:
		var err error
		episodes, args, err = s.consolidationEpisodesTx(ctx, tx, job.Request.Reference, job.ID, job.Request.InputDigest)
		if err != nil {
			return false, err
		}
		if episodes == "" {
			return true, nil
		}
	case skillmaintenance.SkillMatch:
		episodes = `SELECT p.episode_id FROM df_learning_matches m JOIN df_learning_pattern_members p
 ON p.pattern_id=m.pattern_id AND p.revision=m.revision WHERE m.id=? AND m.job_id=?`
		args = []any{job.Request.Reference, job.ID}
	case skillmaintenance.SkillGeneration:
		var reviewed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_generation_inputs i JOIN df_skill_proposals p
 ON p.id=i.proposal_id WHERE i.job_id=? AND p.status<>'validating')`, job.ID).Scan(&reviewed); err != nil {
			return false, err
		}
		// Review is already durable; its ACK must not wait on a later task or
		// reauthorize publication against a changed installed head.
		if reviewed {
			return true, nil
		}
		return s.PromotionReadyTx(ctx, tx, job.ID)
	default:
		return true, nil
	}
	return episodesReadyTx(ctx, tx, episodes, args...)
}

// Review readiness excludes the terminal-ACK exception. A candidate cannot
// publish during source activity, but waiting alone does not make it stale.
func (s *Store) PromotionReadyTx(ctx context.Context, tx *sql.Tx, jobID string) (bool, error) {
	episodes := `SELECT p.episode_id FROM df_learning_promotion_dependencies dep JOIN (
 SELECT id,pattern_id,revision FROM df_learning_matches
 UNION ALL SELECT id,pattern_id,revision FROM df_learning_adjudications) m
 ON m.id=dep.match_id AND m.pattern_id=dep.pattern_id JOIN df_learning_pattern_members p
 ON p.pattern_id=m.pattern_id AND p.revision=m.revision WHERE dep.job_id=? AND dep.revoked=0`
	args := []any{jobID}
	var reference, consolidationJob, sha string
	err := tx.QueryRowContext(ctx, `SELECT c.id,c.job_id,j.input_digest FROM df_learning_promotion_dependencies dep JOIN (
 SELECT id,pattern_id,revision FROM df_learning_matches
 UNION ALL SELECT id,pattern_id,revision FROM df_learning_adjudications) m
 ON m.id=dep.match_id AND m.pattern_id=dep.pattern_id JOIN df_learning_pattern_revisions r
 ON r.pattern_id=m.pattern_id AND r.revision=m.revision JOIN df_learning_consolidations c ON c.id=r.consolidation_id
 JOIN df_skill_maintenance_jobs j ON j.id=c.job_id WHERE dep.job_id=? AND dep.revoked=0`, jobID).Scan(&reference, &consolidationJob, &sha)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		// Promotion also revalidates the original consolidation shortlist,
		// including negative comparisons, not only the selected members.
		shortlist, pins, err := s.consolidationEpisodesTx(ctx, tx, reference, consolidationJob, sha)
		if err != nil {
			return false, err
		}
		if shortlist != "" {
			episodes += " UNION " + shortlist
			args = append(args, pins...)
		}
	}
	return episodesReadyTx(ctx, tx, episodes, args...)
}

func (s *Store) consolidationEpisodesTx(ctx context.Context, tx *sql.Tx, reference, jobID, inputSHA string) (string, []any, error) {
	var data []byte
	var sha string
	err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_learning_consolidations WHERE id=? AND job_id=?`, reference, jobID).Scan(&data, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var snap consolidationSnapshot
	if len(data) > s.consolidationInputLimit()+(32<<10) || digest(data) != sha || sha != inputSHA || json.Unmarshal(data, &snap) != nil || snap.ID != reference || len(snap.Patterns) > 8 {
		return "", nil, nil
	}
	episodes := `SELECT episode_id FROM df_learning_experiences WHERE id=?`
	args := []any{snap.Seed.ID}
	for _, dep := range snap.Patterns {
		episodes += ` UNION SELECT episode_id FROM df_learning_pattern_members WHERE pattern_id=? AND revision=?`
		args = append(args, dep.ID, dep.Revision)
	}
	return episodes, args, nil
}

func episodesReadyTx(ctx context.Context, tx *sql.Tx, episodes string, args ...any) (bool, error) {
	var busy bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_learning_inputs i JOIN harness_runs r ON r.id=i.run_id
 WHERE i.episode_id IN (`+episodes+`) AND r.status NOT IN ('completed','failed','cancelled'))`, args...).Scan(&busy)
	return !busy, err
}

func (s *Store) ExtractionHooks() skillmaintenance.Hooks {
	return skillmaintenance.Hooks{Admission: s.AdmitTx, Ready: s.ReadyTx, CallAdmission: s.AdmitCallTx, Terminal: s.FinalizeTx}
}
