package learning

import (
	"context"
	"database/sql"
	"encoding/json"
)

// A cursor is an optimization, never proof of search coverage. Reconstruct the
// contiguous prefix from successful immutable page decisions before advancing.
func verifiedMatchPrefixTx(ctx context.Context, tx *sql.Tx, pattern string, revision, catalog int64, inventory string, want int) error {
	if want == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.payload,m.payload_sha,m.result,m.result_sha,j.input_digest FROM df_learning_matches m JOIN df_skill_maintenance_jobs j ON j.id=m.job_id WHERE m.pattern_id=? AND m.revision=? AND j.state='completed' AND m.result_sha<>'' ORDER BY CAST(json_extract(CAST(m.payload AS TEXT),'$.view.page_offset') AS INTEGER),m.id`, pattern, revision)
	if err != nil {
		return err
	}
	defer rows.Close()
	prefix := 0
	for rows.Next() {
		var raw, result []byte
		var sha, resultSHA, inputSHA string
		if err = rows.Scan(&raw, &sha, &result, &resultSHA, &inputSHA); err != nil {
			return err
		}
		var snap matchSnapshot
		var out matchResult
		if digest(raw) != sha || sha != inputSHA || digest(result) != resultSHA || json.Unmarshal(raw, &snap) != nil || json.Unmarshal(result, &out) != nil {
			return invalid("matching coverage integrity failure")
		}
		if snap.Catalog != catalog || snap.InventorySHA != inventory || snap.View.PageOffset < prefix {
			continue
		}
		if snap.View.PageOffset != prefix || out.Decision.Relation != "search_incomplete" || len(snap.Choices) == 0 {
			break
		}
		if _, err = validateMatchDecision(snap.View, out.Decision); err != nil {
			return err
		}
		prefix += len(snap.Choices)
		if prefix == want {
			return nil
		}
		if prefix > want {
			break
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return invalid("search cursor has no complete committed coverage")
}
