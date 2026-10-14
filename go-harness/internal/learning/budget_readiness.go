package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

// Soft shares affect dispatch, never billing or authorization. Empty stages
// lend their unused share. Existing hard per-call reservations remain the
// authority; in-flight calls may cross a soft share but not the hard limit.
func (s *Store) budgetReadyTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) (bool, error) {
	table := ""
	switch job.Request.Kind {
	case skillmaintenance.ExperienceExtract:
		table = "df_learning_extractions"
	case skillmaintenance.ExperienceConsolidate:
		table = "df_learning_consolidations"
	case skillmaintenance.SkillMatch:
		table = "df_learning_matches"
	default:
		return true, nil
	}
	var payload []byte
	var sha, result string
	err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha,result_sha FROM `+table+` WHERE id=? AND job_id=?`, job.Request.Reference, job.ID).Scan(&payload, &sha, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var bounds struct {
		Calls  int64 `json:"max_daily_model_calls"`
		Tokens int64 `json:"max_daily_tokens"`
	}
	// A committed output must reach its existing ACK/validation path even when
	// quota is exhausted. Bad records likewise reach ordinary fail-closed guards.
	if result != "" || len(payload) > (2<<20)+(32<<10) || digest(payload) != sha || sha != job.Request.InputDigest || json.Unmarshal(payload, &bounds) != nil || bounds.Calls < 1 || bounds.Tokens < 1 {
		return true, nil
	}
	maxCalls := min(bounds.Calls, s.options.MaxDailyModelCalls, job.Policy.MaxDailyModelCalls)
	maxTokens := min(bounds.Tokens, s.options.MaxDailyTokens, job.Policy.MaxDailyTokens)
	day := s.now().UTC().Format("2006-01-02")
	var totalCalls, totalTokens, reservedCalls, reservedTokens int64
	for _, stage := range []struct {
		kind  skillmaintenance.JobKind
		share int64
	}{
		{skillmaintenance.ExperienceExtract, 40}, {skillmaintenance.ExperienceConsolidate, 30}, {skillmaintenance.SkillMatch, 30},
	} {
		var calls, tokens int64
		if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(c.charged_tokens),0) FROM df_skill_maintenance_calls c
 JOIN df_skill_maintenance_jobs j ON j.id=c.job_id WHERE c.day=? AND j.job_kind=?`, day, stage.kind).Scan(&calls, &tokens); err != nil {
			return false, err
		}
		totalCalls += calls
		totalTokens += tokens
		// Protect downstream work, not upstream backlog. Otherwise one large
		// charged call can leave every stage waiting for another stage's share.
		if stage.kind == job.Request.Kind || job.Request.Kind == skillmaintenance.SkillMatch ||
			job.Request.Kind == skillmaintenance.ExperienceConsolidate && stage.kind == skillmaintenance.ExperienceExtract {
			continue
		}
		var waiting bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_maintenance_jobs WHERE job_kind=? AND state IN ('queued','running','retry'))`, stage.kind).Scan(&waiting); err != nil {
			return false, err
		}
		if waiting {
			reservedCalls += max(0, maxCalls*stage.share/100-calls)
			reservedTokens += max(0, maxTokens*stage.share/100-tokens)
		}
	}
	return totalCalls < maxCalls-reservedCalls && totalTokens < maxTokens-reservedTokens, nil
}
