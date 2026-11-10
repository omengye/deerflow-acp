package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Keep operational failures as measurement evidence. Their independent counter
// survives scheduler slices and restart; a valid completed comparison is reused.
func recordMeasurementAttemptTx(ctx context.Context, tx *sql.Tx, evaluation, key string, r harness.SkillBehaviorResult) (bool, error) {
	switch r.ErrorCode {
	case "model_error", "timeout", "workspace_error":
	default:
		return false, nil
	}
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt),0)+1 FROM df_skill_measurement_attempts WHERE evaluation_id=? AND result_key=?`, evaluation, key).Scan(&attempt); err != nil {
		return false, err
	}
	raw, _ := json.Marshal(r)
	if _, err := tx.ExecContext(ctx, `INSERT INTO df_skill_measurement_attempts VALUES(?,?,?,?,?)`, evaluation, key, attempt, raw, hash(raw)); err != nil {
		return false, err
	}
	return attempt < 3, nil
}
