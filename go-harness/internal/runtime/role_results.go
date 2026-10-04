package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

const roleResultNotification bt.NotificationKind = "role_result"

type roleResult struct {
	SourceRunID     string `json:"sourceRunId"`
	SourceSHA       string `json:"sourceSha"`
	RoleID          string `json:"roleId"`
	RecipientRole   string `json:"recipientRole"`
	Succeeded       bool   `json:"succeeded"`
	Result          string `json:"result"`
	ContextRevision int64  `json:"contextRevision,omitempty"`
	ContextDigest   string `json:"contextDigest,omitempty"`
}

type roleResultDepthKey struct{}

func migrateRoleResults(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_role_results (
 notification_id TEXT PRIMARY KEY,source_run_id TEXT UNIQUE NOT NULL REFERENCES harness_runs(id),
 session_id TEXT NOT NULL,root_id TEXT NOT NULL,digest TEXT NOT NULL,payload BLOB NOT NULL);`)
	return err
}

// Only a saved private continuation can produce a parent-facing summary. The
// private transcript remains in its role context and never enters this inbox.
func publishRoleResultTx(ctx context.Context, tx *sql.Tx, state harness.ExecutionState, succeeded bool) error {
	if state.Status != harness.ExecutionCompleted && state.Status != harness.ExecutionFailed {
		return nil
	}
	row, err := readExecution(ctx, tx, state.SessionID, state.RunID)
	if err != nil {
		return err
	}
	source, err := loadContinuationSource(ctx, tx, row)
	if err != nil || source == nil || source.ParentRole == nil {
		return err
	}
	role := source.ParentRole.Context.Invocation
	result := roleResult{SourceRunID: state.RunID, SourceSHA: row.SourceSHA, RoleID: role.ID, RecipientRole: role.ParentID, Succeeded: succeeded,
		Result: "The delegated continuation failed. Review its execution before deciding whether to retry."}
	if succeeded {
		saved, err := readRoleContextTx(ctx, tx, source.RootBudgetID, role.ID, 0)
		if err != nil {
			return err
		}
		if saved.Context.RunID != state.RunID || saved.Revision <= source.ParentRole.Revision {
			return continuationConflict("completed role did not save its new context")
		}
		result.ContextRevision, result.ContextDigest = saved.Revision, saved.Digest
		result.Result, err = roleContextSummary(saved)
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	id := "role-result/" + state.RunID
	n := bt.Notification{ID: id, TaskID: source.TaskID, SessionID: state.SessionID, Kind: roleResultNotification, Version: source.Task.Version, Data: data, CreatedAt: time.Now().UTC()}
	payload, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_role_results(notification_id,source_run_id,session_id,root_id,digest,payload) VALUES(?,?,?,?,?,?)`, id, state.RunID, state.SessionID, source.RootBudgetID, executionDigest(data), data); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)`, id, state.SessionID, source.TaskID, payload)
	return err
}

func readRoleResultTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec, n bt.Notification) (roleResult, error) {
	var result roleResult
	depth, _ := ctx.Value(roleResultDepthKey{}).(int)
	if depth >= 3 {
		return result, continuationConflict("role result ancestry exceeds depth limit")
	}
	ctx = context.WithValue(ctx, roleResultDepthKey{}, depth+1)
	var data []byte
	var run, session, root, digest string
	if err := tx.QueryRowContext(ctx, `SELECT source_run_id,session_id,root_id,digest,payload FROM harness_role_results WHERE notification_id=?`, n.ID).Scan(&run, &session, &root, &digest, &data); err != nil {
		return result, err
	}
	if executionDigest(data) != digest || !bytes.Equal(data, n.Data) || json.Unmarshal(data, &result) != nil || result.SourceRunID != run || session != b.ParentSessionID || root != b.RootBudgetID || n.TaskID != b.TaskID || n.SessionID != session {
		return result, continuationConflict("role result identity changed")
	}
	row, err := readExecution(ctx, tx, session, run)
	if err != nil {
		return result, err
	}
	if row.SourceSHA != result.SourceSHA || (row.State.Status != harness.ExecutionCompleted && row.State.Status != harness.ExecutionFailed) {
		return result, continuationConflict("role result execution changed")
	}
	source, err := loadContinuationSource(ctx, tx, row)
	if err != nil {
		return result, err
	}
	if source == nil || source.ParentRole == nil || source.Binding != b || source.ParentRole.Context.Invocation.ID != result.RoleID || source.ParentRole.Context.Invocation.ParentID != result.RecipientRole || source.Task.Version != n.Version {
		return result, continuationConflict("role result ancestry changed")
	}
	if result.Succeeded {
		if row.State.Status != harness.ExecutionCompleted || result.ContextRevision <= source.ParentRole.Revision {
			return result, continuationConflict("role result completion changed")
		}
		saved, err := readRoleContextTx(ctx, tx, root, result.RoleID, result.ContextRevision)
		if err != nil {
			return result, err
		}
		summary, err := roleContextSummary(saved)
		if err != nil || saved.Digest != result.ContextDigest || saved.Context.RunID != run || summary != result.Result {
			return result, continuationConflict("role result conversation changed")
		}
	} else if result.ContextRevision != 0 || result.ContextDigest != "" {
		return result, continuationConflict("failed role result references a completed conversation")
	}
	original, _ := json.Marshal(source.Spec)
	current, _ := json.Marshal(spec)
	if !bytes.Equal(original, current) {
		return result, continuationConflict("role result policy changed")
	}
	return result, nil
}

func roleContextSummary(saved savedRoleContext) (string, error) {
	var messages []*schema.Message
	if json.Unmarshal(saved.Context.Messages, &messages) != nil || len(messages) == 0 {
		return "", continuationConflict("completed role has no result")
	}
	last := messages[len(messages)-1]
	if last == nil || len(last.ToolCalls) != 0 {
		return "", continuationConflict("completed role result is not terminal")
	}
	if last.Content == "" {
		return "The delegated continuation completed with a non-text result. Inspect its execution artifacts.", nil
	}
	return last.Content, nil
}
