package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func migrateGoals(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_goals (
 session_id TEXT PRIMARY KEY REFERENCES harness_sessions(id) ON DELETE CASCADE,
 id TEXT NOT NULL, objective TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS harness_goal_runs (
 run_id TEXT PRIMARY KEY REFERENCES harness_runs(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 goal_id TEXT NOT NULL, objective TEXT NOT NULL, continuations INTEGER NOT NULL DEFAULT 0,
 progress TEXT NOT NULL DEFAULT '', evaluation BLOB);
CREATE INDEX IF NOT EXISTS harness_goal_runs_goal ON harness_goal_runs(session_id,goal_id);`)
	return err
}

func (s *Store) Goal(ctx context.Context, session string) (*harness.Goal, error) {
	var g harness.Goal
	err := s.db.QueryRowContext(ctx, `SELECT id,objective FROM harness_goals WHERE session_id=?`, session).Scan(&g.ID, &g.Objective)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.Status = "active"
	var complete bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_goal_runs g JOIN harness_runs r ON r.id=g.run_id WHERE g.session_id=? AND g.goal_id=? AND r.status='completed' AND r.stop_reason='end_turn' AND json_extract(g.evaluation,'$.status')='achieved' AND NOT EXISTS(SELECT 1 FROM harness_tool_receipts t WHERE t.run_id=r.id AND t.state IN ('pending','started','uncertain')))`, session, g.ID).Scan(&complete)
	if complete {
		g.Status = "achieved"
	}
	return &g, err
}

func (s *Store) GoalForRun(ctx context.Context, req harness.RunRequest) (*harness.GoalAttempt, error) {
	var a harness.GoalAttempt
	err := s.db.QueryRowContext(ctx, `SELECT goal_id,objective,continuations,progress FROM harness_goal_runs WHERE run_id=? AND session_id=?`, req.RunID, req.Session.ID).Scan(&a.Goal.ID, &a.Goal.Objective, &a.Continuations, &a.LastProgress)
	if err == nil {
		a.Goal.Status = "active"
		return &a, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	g, err := s.Goal(ctx, req.Session.ID)
	if err != nil || g == nil || g.Status != "active" {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO harness_goal_runs(run_id,session_id,goal_id,objective) VALUES(?,?,?,?)`, req.RunID, req.Session.ID, g.ID, g.Objective)
	if err != nil {
		return nil, err
	}
	a.Goal = *g
	return &a, nil
}

func (s *Store) RecordGoalEvaluation(ctx context.Context, req harness.RunRequest, a harness.GoalAttempt, evaluation harness.GoalEvaluation, progress string, continued bool) error {
	// Model judgment cannot override an uncertain or failed native receipt.
	// In particular, a rejected permission is not an invitation to retry it.
	if continued || evaluation.Status == "achieved" {
		var blocked bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE run_id=? AND (state IN ('pending','started','uncertain','failed','not_executed') OR COALESCE(json_extract(receipt,'$.error'),'')<>''))`, req.RunID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return fmt.Errorf("%w: goal evaluation cannot override incomplete or failed tool receipts", harness.ErrReconciliationRequired)
		}
	}
	raw, err := json.Marshal(evaluation)
	if err != nil {
		return err
	}
	n := 0
	if continued {
		n = 1
	}
	r, err := s.db.ExecContext(ctx, `UPDATE harness_goal_runs SET evaluation=?,progress=?,continuations=continuations+? WHERE run_id=? AND session_id=? AND goal_id=? AND continuations=?`, raw, progress, n, req.RunID, req.Session.ID, a.Goal.ID, a.Continuations)
	if err != nil {
		return err
	}
	rows, err := r.RowsAffected()
	if err == nil && rows != 1 {
		return harness.ErrExecutionConflict
	}
	return err
}

// This is parsed only from an entire user prompt, never from tool output or
// model-generated text. It remains under the normal foreground admission lease.
func (s *Service) goalCommand(ctx context.Context, id string, input []harness.Content, emit harness.EventHandler) (bool, error) {
	if len(input) != 1 || input[0].Type != "text" {
		return false, nil
	}
	text := strings.TrimSpace(input[0].Text)
	if text != "/goal" && !strings.HasPrefix(text, "/goal ") {
		return false, nil
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, "/goal"))
	if arg != "" {
		if err := s.Store.requireNoWaitingExecution(ctx, id); err != nil {
			return true, err
		}
		if arg == "clear" {
			if _, err := s.Store.db.ExecContext(ctx, `DELETE FROM harness_goals WHERE session_id=?`, id); err != nil {
				return true, err
			}
		} else {
			if !utf8.ValidString(arg) || len(arg) > 8192 {
				return true, fmt.Errorf("%w: goal must be UTF-8 and at most 8192 bytes", harness.ErrInvalidInput)
			}
			if _, err := s.Store.db.ExecContext(ctx, `INSERT INTO harness_goals(session_id,id,objective) VALUES(?,?,?) ON CONFLICT(session_id) DO UPDATE SET id=excluded.id,objective=excluded.objective`, id, NewID(), arg); err != nil {
				return true, err
			}
		}
	}
	g, err := s.Store.Goal(ctx, id)
	if err != nil {
		return true, err
	}
	message := "No active goal. Use /goal <objective> to set one; /goal clear to remove it."
	if g != nil {
		message = fmt.Sprintf("Goal (%s): %s\nThe next prompt will use this objective. Evaluation is evidence-based but not a correctness guarantee; normal permission and budget limits still apply.", g.Status, g.Objective)
	}
	e, err := s.Store.Append(ctx, harness.RunEvent{SessionID: id, Kind: "text_delta", Text: message}, nil)
	if err == nil && emit != nil {
		err = emit(ctx, e)
	}
	return true, err
}
