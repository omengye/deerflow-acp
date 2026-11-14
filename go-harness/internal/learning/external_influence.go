package learning

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ExternalInfluenceTx reports, per supporting experience of a pattern
// revision, whether its source episode consumed third-party content (web,
// browser, MCP or attachment results). Those result bodies never reach
// extraction, but they can steer the assistant text and later local actions
// that do, so a procedure learned mainly from such episodes is externally
// derived. Unreadable evidence counts as external (fail closed).
func (s *Store) ExternalInfluenceTx(ctx context.Context, tx *sql.Tx, patternID string, revision int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.experience_id,x.payload FROM df_learning_pattern_members m
 JOIN df_learning_experiences e ON e.id=m.experience_id JOIN df_learning_extractions x ON x.id=e.extraction_id
 WHERE m.pattern_id=? AND m.revision<=? ORDER BY m.experience_id`, patternID, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var payload []byte
		if err = rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		out[id] = externallyInfluenced(payload)
	}
	return out, rows.Err()
}

func externallyInfluenced(payload []byte) bool {
	var snap extractionSnapshot
	if json.Unmarshal(payload, &snap) != nil {
		return true
	}
	for _, ref := range snap.View.References {
		if ref.Kind == "tool_observation" && harness.ExternalContentTool(ref.ToolName) {
			return true
		}
	}
	return false
}
