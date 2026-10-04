package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func migrateRoleContexts(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_role_context_stages (
 run_id TEXT NOT NULL REFERENCES harness_runs(id),attempt_id TEXT NOT NULL,
 role_id TEXT NOT NULL,payload BLOB NOT NULL,PRIMARY KEY(run_id,attempt_id,role_id));
CREATE TABLE IF NOT EXISTS harness_role_contexts (
 role_id TEXT NOT NULL,revision INTEGER NOT NULL,root_id TEXT NOT NULL,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id),run_id TEXT NOT NULL REFERENCES harness_runs(id),
 attempt_id TEXT NOT NULL,digest TEXT NOT NULL,payload BLOB NOT NULL,
 PRIMARY KEY(role_id,revision),UNIQUE(role_id,attempt_id));
CREATE INDEX IF NOT EXISTS harness_role_contexts_session ON harness_role_contexts(session_id);`)
	return err
}

type savedRoleContext struct {
	Revision int64                   `json:"revision"`
	Digest   string                  `json:"digest"`
	Context  interaction.RoleContext `json:"context"`
}

func validateRoleContext(ctx context.Context, tx *sql.Tx, scope budget.Scope, state interaction.RoleContext) error {
	if state.RunID != scope.MemberID || state.SessionID != scope.SessionID || state.Invocation.RootID != scope.RootBudgetID || state.Invocation.ID == "" || len(state.Messages) == 0 || len(state.Messages) > 64<<20 {
		return harness.ErrExecutionConflict
	}
	var messages []*schema.Message
	if err := json.Unmarshal(state.Messages, &messages); err != nil || len(messages) == 0 {
		return harness.ErrExecutionConflict
	}
	for _, message := range messages {
		if message == nil {
			return harness.ErrExecutionConflict
		}
	}
	invocation, err := agents.ReadInvocation(ctx, tx, state.Invocation.ID, scope.RootBudgetID)
	if err != nil {
		var syntax *json.SyntaxError
		var shape *json.UnmarshalTypeError
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, harness.ErrInvalidInput) || errors.As(err, &syntax) || errors.As(err, &shape) {
			return harness.ErrExecutionConflict
		}
		return err
	}
	saved, _ := json.Marshal(invocation)
	actual, err := json.Marshal(state.Invocation)
	if err != nil || !bytes.Equal(saved, actual) {
		return harness.ErrExecutionConflict
	}
	return nil
}

func stageRoleContextTx(ctx context.Context, tx *sql.Tx, scope budget.Scope, state interaction.RoleContext) error {
	if err := validateRoleContext(ctx, tx, scope, state); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT payload FROM harness_role_context_stages WHERE run_id=? AND attempt_id=? AND role_id=?`, scope.MemberID, scope.AttemptID, state.Invocation.ID).Scan(&previous)
	if err == nil {
		if !bytes.Equal(previous, data) {
			return harness.ErrExecutionConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_role_context_stages(run_id,attempt_id,role_id,payload) VALUES(?,?,?,?)`, scope.MemberID, scope.AttemptID, state.Invocation.ID, data)
	return err
}

func (a *executionAttempt) StageRoleContext(ctx context.Context, state interaction.RoleContext) error {
	if err := a.service.Coordinator.Authorize(a.lease.Scope.SessionID, a.lease.OwnerID); err != nil {
		return err
	}
	return withExecutionTransaction(ctx, a.service.Store, func(tx *sql.Tx) error {
		if _, err := checkExecutionLease(ctx, tx, a.lease); err != nil {
			return err
		}
		return stageRoleContextTx(ctx, tx, a.lease.Scope, state)
	})
}

func (a *BackgroundInteractionAttempt) StageRoleContext(ctx context.Context, state interaction.RoleContext) error {
	return withExecutionTransaction(ctx, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(ctx, tx, false); err != nil {
			return err
		}
		allowed, err := agents.IsDescendant(ctx, tx, a.scope.Binding.RootBudgetID, a.scope.Binding.RoleID(), state.Invocation.ID)
		if err != nil {
			return err
		}
		if !allowed {
			return harness.ErrPermissionDenied
		}
		return stageRoleContextTx(ctx, tx, background.BudgetScope(a.scope), state)
	})
}

// Settle only in the transaction publishing joined execution progress. A
// staged AfterAgent callback cannot make an unsuccessful attempt resumable.
func settleRoleContextsTx(ctx context.Context, tx *sql.Tx, scope budget.Scope, publish bool) error {
	if publish {
		rows, err := tx.QueryContext(ctx, `SELECT role_id,payload FROM harness_role_context_stages WHERE run_id=? AND attempt_id=? ORDER BY role_id`, scope.MemberID, scope.AttemptID)
		if err != nil {
			return err
		}
		type item struct {
			id   string
			data []byte
		}
		var stages []item
		for rows.Next() {
			var stage item
			if err = rows.Scan(&stage.id, &stage.data); err != nil {
				break
			}
			stages = append(stages, stage)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, stage := range stages {
			var state interaction.RoleContext
			if err = json.Unmarshal(stage.data, &state); err != nil {
				return err
			}
			if stage.id != state.Invocation.ID {
				return harness.ErrExecutionConflict
			}
			if err = validateRoleContext(ctx, tx, scope, state); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO harness_role_contexts(role_id,revision,root_id,session_id,run_id,attempt_id,digest,payload) SELECT ?,COALESCE(MAX(revision),0)+1,?,?,?,?,?,? FROM harness_role_contexts WHERE role_id=?`, stage.id, scope.RootBudgetID, scope.SessionID, scope.MemberID, scope.AttemptID, executionDigest(stage.data), stage.data, stage.id); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM harness_role_context_stages WHERE run_id=? AND attempt_id=?`, scope.MemberID, scope.AttemptID)
	return err
}

func readRoleContextTx(ctx context.Context, tx *sql.Tx, root, id string, revision int64) (savedRoleContext, error) {
	var saved savedRoleContext
	var data []byte
	var sessionID, runID, attemptID string
	query := `SELECT revision,digest,payload,session_id,run_id,attempt_id FROM harness_role_contexts WHERE role_id=? AND root_id=?`
	args := []any{id, root}
	if revision > 0 {
		query += ` AND revision=?`
		args = append(args, revision)
	}
	query += ` ORDER BY revision DESC LIMIT 1`
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&saved.Revision, &saved.Digest, &data, &sessionID, &runID, &attemptID); err != nil {
		return saved, err
	}
	if executionDigest(data) != saved.Digest || json.Unmarshal(data, &saved.Context) != nil || saved.Revision < 1 || saved.Context.Invocation.ID != id {
		return saved, harness.ErrExecutionConflict
	}
	err := validateRoleContext(ctx, tx, budget.Scope{RootBudgetID: root, MemberID: runID, SessionID: sessionID, AttemptID: attemptID}, saved.Context)
	return saved, err
}
