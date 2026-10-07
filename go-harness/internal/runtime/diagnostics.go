package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SessionDiagnostics is an authenticated host projection over bounded metadata.
// It does not take a foreground execution slot or expose model/tool content.
func (s *Service) SessionDiagnostics(ctx context.Context, id string) (map[string]any, error) {
	data, err := s.Store.SessionDiagnostics(ctx, id)
	if err != nil {
		return nil, err
	}
	data["phase"] = s.Coordinator.Activity().Phases[id]
	if s.runQueue.isWaiting(id) {
		data["phase"] = "queued"
	}
	return data, nil
}

func (s *Store) SessionDiagnostics(ctx context.Context, id string) (map[string]any, error) {
	if _, err := s.ManagementSession(ctx, id); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := map[string]any{"session_id": id, "sampled_at": time.Now().UTC(), "model_calls": nil, "tool_calls": nil, "spent_tokens": nil, "held_tokens": nil, "blocked_reason": "", "model_timings": []any{}, "tools": []any{}}
	var run, status, reason string
	err = tx.QueryRowContext(ctx, `SELECT id,status,stop_reason FROM harness_runs WHERE session_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, id).Scan(&run, &status, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out["run_id"], out["status"], out["stop_reason"] = run, status, reason
	var root, attempt, execution, blocked string
	err = tx.QueryRowContext(ctx, `SELECT root_budget_id,attempt_id,status,blocked_reason FROM harness_executions WHERE run_id=?`, run).Scan(&root, &attempt, &execution, &blocked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		out["root_budget_id"], out["attempt_id"], out["execution_status"], out["blocked_reason"] = root, attempt, execution, blocked
		var models, tools, spent, held int64
		var budgetReason string
		err = tx.QueryRowContext(ctx, `SELECT model_calls,tool_calls,spent_tokens,held_tokens,blocked_reason FROM budget_roots WHERE id=?`, root).Scan(&models, &tools, &spent, &held, &budgetReason)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			out["model_calls"], out["tool_calls"], out["spent_tokens"], out["held_tokens"] = models, tools, spent, held
			if budgetReason != "" {
				out["blocked_reason"] = budgetReason
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT json_extract(event,'$.timing'),json_extract(event,'$.status') FROM harness_events WHERE run_id=? AND json_extract(event,'$.kind')='model_timing' ORDER BY sequence DESC LIMIT 16`, run)
	if err != nil {
		return nil, err
	}
	var timings []any
	for rows.Next() {
		var raw, status string
		if err = rows.Scan(&raw, &status); err != nil {
			rows.Close()
			return nil, err
		}
		var timing harness.ModelTiming
		if err = json.Unmarshal([]byte(raw), &timing); err != nil {
			rows.Close()
			return nil, err
		}
		timings = append(timings, map[string]any{"timing": timing, "status": status})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if timings != nil {
		out["model_timings"] = timings
	}
	rows, err = tx.QueryContext(ctx, `SELECT json_extract(receipt,'$.toolName'),state,json_extract(receipt,'$.failureCode'),json_extract(receipt,'$.createdAt'),json_extract(receipt,'$.updatedAt') FROM harness_tool_receipts WHERE run_id=? ORDER BY rowid DESC LIMIT 16`, run)
	if err != nil {
		return nil, err
	}
	var tools []any
	for rows.Next() {
		var name, state, created, updated string
		var code sql.NullString
		if err = rows.Scan(&name, &state, &code, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		start, e := time.Parse(time.RFC3339Nano, created)
		if e != nil {
			rows.Close()
			return nil, e
		}
		end, e := time.Parse(time.RFC3339Nano, updated)
		if e != nil {
			rows.Close()
			return nil, e
		}
		tools = append(tools, map[string]any{"name": name, "state": state, "failure_code": code.String, "lifecycle_ms": max(0, end.Sub(start).Milliseconds())})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if tools != nil {
		out["tools"] = tools
	}
	var uncertain int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_tool_receipts WHERE session_id=? AND state IN ('started','uncertain')`, id).Scan(&uncertain); err != nil {
		return nil, err
	}
	out["unreconciled_receipts"] = uncertain
	out["limits"] = map[string]int{"model_timings": 16, "tools": 16}
	return out, nil
}
