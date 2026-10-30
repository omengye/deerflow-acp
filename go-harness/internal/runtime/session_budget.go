package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func migrateSessionBudget(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(harness_session_configs)`)
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var ordinal, required, primary int
		var name, kind string
		var value sql.NullString
		if err = rows.Scan(&ordinal, &name, &kind, &required, &value, &primary); err != nil {
			break
		}
		columns[name] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range []string{"max_run_tokens", "max_run_model_calls", "max_run_tool_calls", "budget_version"} {
		if !columns[column] {
			if _, err = db.ExecContext(ctx, `ALTER TABLE harness_session_configs ADD COLUMN `+column+` INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_session_budget_changes(session_id TEXT NOT NULL REFERENCES harness_sessions(id),operation_id TEXT NOT NULL,request BLOB NOT NULL,response BLOB NOT NULL,PRIMARY KEY(session_id,operation_id))`)
	return err
}

func budgetSelection(l harness.BudgetLimits) harness.TaskBudget {
	return harness.TaskBudget{MaxTokens: l.MaxTokens, MaxModelCalls: l.MaxModelCalls, MaxToolCalls: l.MaxToolCalls}
}

func (s *Store) sessionBudget(x harness.Session) harness.SessionBudget {
	return harness.SessionBudget{SessionID: x.ID, Version: max(x.BudgetVersion, 1), Requested: x.TaskBudget(), Host: budgetSelection(s.BudgetLimits), Effective: budgetSelection(x.EffectiveBudget(s.BudgetLimits))}
}

func (s *Service) SessionBudget(ctx context.Context, owner, id string) (harness.SessionBudget, error) {
	if err := s.Coordinator.Authorize(id, owner); err != nil {
		return harness.SessionBudget{}, err
	}
	x, err := s.Store.Session(ctx, id)
	if err != nil {
		return harness.SessionBudget{}, err
	}
	return s.Store.sessionBudget(x), nil
}

func (s *Service) SetSessionBudget(ctx context.Context, owner string, request harness.SessionBudgetRequest) (harness.SessionBudget, error) {
	if err := s.Coordinator.Authorize(request.SessionID, owner); err != nil {
		return harness.SessionBudget{}, err
	}
	if prior, found, err := s.Store.savedSessionBudgetChange(ctx, request); found || err != nil {
		return prior, err
	}
	ctx, release, err := s.Coordinator.Begin(ctx, request.SessionID, owner)
	if err != nil {
		return harness.SessionBudget{}, err
	}
	defer release()
	if err = s.Store.requireNoWaitingExecution(ctx, request.SessionID); err != nil {
		return harness.SessionBudget{}, err
	}
	return s.Store.setSessionBudget(ctx, request)
}

func (s *Store) savedSessionBudgetChange(ctx context.Context, request harness.SessionBudgetRequest) (harness.SessionBudget, bool, error) {
	var result harness.SessionBudget
	var saved, response []byte
	err := s.db.QueryRowContext(ctx, `SELECT request,response FROM harness_session_budget_changes WHERE session_id=? AND operation_id=?`, request.SessionID, request.OperationID).Scan(&saved, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return result, true, err
	}
	if string(saved) != string(encoded) {
		return result, true, harness.ErrExecutionConflict
	}
	err = json.Unmarshal(response, &result)
	return result, true, err
}

func (s *Store) setSessionBudget(ctx context.Context, request harness.SessionBudgetRequest) (harness.SessionBudget, error) {
	var zero harness.SessionBudget
	if request.SessionID == "" || request.OperationID == "" || len(request.OperationID) > 128 || request.ExpectedVersion < 1 {
		return zero, harness.ErrInvalidInput
	}
	limits, host := request.Limits, s.BudgetLimits
	if limits.MaxTokens < 0 || limits.MaxModelCalls < 0 || limits.MaxToolCalls < 0 {
		return zero, harness.ErrInvalidInput
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return zero, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE harness_sessions SET updated_at=updated_at WHERE id=?`, request.SessionID); err != nil {
		return zero, err
	}
	var saved, response []byte
	err = tx.QueryRowContext(ctx, `SELECT request,response FROM harness_session_budget_changes WHERE session_id=? AND operation_id=?`, request.SessionID, request.OperationID).Scan(&saved, &response)
	if err == nil {
		if string(saved) != string(encoded) {
			return zero, harness.ErrExecutionConflict
		}
		err = json.Unmarshal(response, &zero)
		return zero, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	if host.MaxTokens > 0 && limits.MaxTokens > host.MaxTokens || host.MaxModelCalls > 0 && limits.MaxModelCalls > host.MaxModelCalls || host.MaxToolCalls > 0 && limits.MaxToolCalls > host.MaxToolCalls {
		return zero, fmt.Errorf("%w: task budget cannot exceed host limits", harness.ErrInvalidInput)
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_sessions WHERE id=?)`, request.SessionID).Scan(&exists); err != nil {
		return zero, err
	}
	if !exists {
		return zero, harness.ErrNotFound
	}
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT NULLIF(budget_version,0) FROM harness_session_configs WHERE session_id=?),1)`, request.SessionID).Scan(&version); err != nil {
		return zero, err
	}
	if version != request.ExpectedVersion {
		return zero, harness.ErrExecutionConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_session_configs(session_id,budget_version,max_run_tokens,max_run_model_calls,max_run_tool_calls) VALUES(?,?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET budget_version=excluded.budget_version,max_run_tokens=excluded.max_run_tokens,max_run_model_calls=excluded.max_run_model_calls,max_run_tool_calls=excluded.max_run_tool_calls`, request.SessionID, version+1, limits.MaxTokens, limits.MaxModelCalls, limits.MaxToolCalls)
	if err != nil {
		return zero, err
	}
	x := harness.Session{ID: request.SessionID, BudgetVersion: version + 1, MaxRunTokens: limits.MaxTokens, MaxRunModelCalls: limits.MaxModelCalls, MaxRunToolCalls: limits.MaxToolCalls}
	result := s.sessionBudget(x)
	response, err = json.Marshal(result)
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_session_budget_changes(session_id,operation_id,request,response) VALUES(?,?,?,?)`, request.SessionID, request.OperationID, encoded, response); err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_sessions SET updated_at=? WHERE id=?`, timestamp(), request.SessionID); err != nil {
		return zero, err
	}
	return result, tx.Commit()
}
