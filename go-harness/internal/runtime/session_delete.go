package runtime

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// purgeSession removes a detached foreground session and its private durable
// state in one transaction. The caller must hold a coordinator deletion fence.
// Background parents remain protected until their task graph has a separate
// retention policy; an accidental partial graph purge would strand children.
func (s *Store) purgeSession(ctx context.Context, id string) (bool, []string, error) {
	if id == "" {
		return false, nil, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_sessions WHERE id=?)`, id).Scan(&exists); err != nil {
		return false, nil, err
	}
	if !exists {
		return true, nil, nil
	}
	tables, err := sessionTables(ctx, tx)
	if err != nil {
		return false, nil, err
	}
	if tables["harness_background_specs"] {
		var child bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs WHERE child_session_id=?)`, id).Scan(&child); err != nil {
			return false, nil, err
		}
		if child {
			return false, nil, harness.ErrInvalidInput
		}
	}
	if tables["harness_background_bindings"] {
		var related bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_bindings WHERE parent_session_id=? OR child_session_id=?)`, id, id).Scan(&related); err != nil {
			return false, nil, err
		}
		if related {
			return false, nil, harness.ErrBusy
		}
	}
	var unresolved bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('pending','started','uncertain'))`, id).Scan(&unresolved); err != nil {
		return false, nil, err
	}
	if unresolved {
		return false, nil, harness.ErrReceiptConflict
	}
	var paths []string
	if tables["harness_assets"] {
		rows, e := tx.QueryContext(ctx, `SELECT path FROM harness_assets WHERE session_id=?`, id)
		if e != nil {
			return false, nil, e
		}
		for rows.Next() {
			var path string
			if e = rows.Scan(&path); e != nil {
				break
			}
			paths = append(paths, path)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return false, nil, e
		}
	}
	// Delete leaf rows before their parents. Optional subsystems create tables
	// only when configured; every query here has a fixed, audited table name.
	steps := []struct{ table, query string }{
		{"memory_facts_fts", `DELETE FROM memory_facts_fts WHERE scope_key IN (SELECT key FROM memory_scopes WHERE kind='session' AND subject=?)`},
		{"memory_fts_state", `DELETE FROM memory_fts_state WHERE scope_key IN (SELECT key FROM memory_scopes WHERE kind='session' AND subject=?)`},
		{"memory_fact_revisions", `DELETE FROM memory_fact_revisions WHERE fact_id IN (SELECT f.id FROM memory_facts f JOIN memory_scopes sc ON sc.key=f.scope_key WHERE sc.kind='session' AND sc.subject=?)`},
		{"memory_facts", `DELETE FROM memory_facts WHERE scope_key IN (SELECT key FROM memory_scopes WHERE kind='session' AND subject=?)`},
		{"memory_staged_facts", `DELETE FROM memory_staged_facts WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"memory_extraction_audit", `DELETE FROM memory_extraction_audit WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"memory_scopes", `DELETE FROM memory_scopes WHERE kind='session' AND subject=?`},
		{"harness_artifacts", `DELETE FROM harness_artifacts WHERE session_id=?`},
		{"harness_input_assets", `DELETE FROM harness_input_assets WHERE input_id IN (SELECT i.id FROM harness_inputs i JOIN harness_runs r ON r.id=i.run_id WHERE r.session_id=?)`},
		{"harness_assets", `DELETE FROM harness_assets WHERE session_id=?`},
		{"budget_tool_continuations", `DELETE FROM budget_tool_continuations WHERE parent_id IN (SELECT v.id FROM budget_reservations v JOIN budget_roots r ON r.id=v.root_id WHERE r.session_id=?) OR child_id IN (SELECT v.id FROM budget_reservations v JOIN budget_roots r ON r.id=v.root_id WHERE r.session_id=?)`},
		{"budget_reservations", `DELETE FROM budget_reservations WHERE root_id IN (SELECT id FROM budget_roots WHERE session_id=?)`},
		{"budget_attempts", `DELETE FROM budget_attempts WHERE root_id IN (SELECT id FROM budget_roots WHERE session_id=?)`},
		{"budget_members", `DELETE FROM budget_members WHERE root_id IN (SELECT id FROM budget_roots WHERE session_id=?)`},
		{"budget_roots", `DELETE FROM budget_roots WHERE session_id=?`},
		{"harness_execution_grants", `DELETE FROM harness_execution_grants WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"harness_execution_audits", `DELETE FROM harness_execution_audits WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"harness_execution_intents", `DELETE FROM harness_execution_intents WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"harness_execution_attempts", `DELETE FROM harness_execution_attempts WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"harness_continuation_sources", `DELETE FROM harness_continuation_sources WHERE parent_session_id=?`},
		{"harness_executions", `DELETE FROM harness_executions WHERE session_id=?`},
		{"harness_tool_reconciliations", `DELETE FROM harness_tool_reconciliations WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"harness_tool_receipts", `DELETE FROM harness_tool_receipts WHERE session_id=?`},
		{"harness_approvals", `DELETE FROM harness_approvals WHERE session_id=?`},
		{"eino_checkpoints", `DELETE FROM eino_checkpoints WHERE id IN (SELECT 'harness/turn/v1/'||id FROM harness_runs WHERE session_id=?)`},
		{"eino_session_events", `DELETE FROM eino_session_events WHERE session_id=?`},
		{"harness_events", `DELETE FROM harness_events WHERE session_id=?`},
		{"harness_inputs", `DELETE FROM harness_inputs WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`},
		{"harness_runs", `DELETE FROM harness_runs WHERE session_id=?`},
		{"harness_session_configs", `DELETE FROM harness_session_configs WHERE session_id=?`},
		{"harness_sessions", `DELETE FROM harness_sessions WHERE id=?`},
	}
	for _, step := range steps {
		if !tables[step.table] {
			continue
		}
		// Only the budget continuation statement has two placeholders.
		if step.table == "budget_tool_continuations" {
			_, err = tx.ExecContext(ctx, step.query, id, id)
		} else {
			_, err = tx.ExecContext(ctx, step.query, id)
		}
		if err != nil {
			return false, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, nil, err
	}
	return false, paths, nil
}

func sessionTables(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

func (s *Service) DeleteSession(ctx context.Context, id string) (bool, error) {
	release, err := s.Coordinator.ReserveCleanup(id)
	if err != nil {
		return false, err
	}
	defer release()
	alreadyDeleted, paths, err := s.Store.purgeSession(ctx, id)
	if err != nil {
		return false, err
	}
	if s.Assets != nil {
		if alreadyDeleted {
			err = s.Assets.CleanupOrphans(ctx)
		} else {
			err = s.Assets.RemoveOrphans(ctx, paths)
		}
		if err != nil {
			return alreadyDeleted, err
		}
	}
	return alreadyDeleted, nil
}

// DeleteAttachedSession handles ACP deletion of an idle session owned by the
// calling connection. A detached session follows the management path. Once
// admitted, resource release and purge finish under a bounded cleanup context
// even if the client disconnects while waiting for its response.
func (s *Service) DeleteAttachedSession(ctx context.Context, owner, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	finish, err := s.Coordinator.ReserveOwnerCleanup(id, owner)
	if errors.Is(err, harness.ErrNotAttached) {
		return s.DeleteSession(ctx, id)
	}
	if err != nil {
		return false, err
	}
	restore := true
	defer func() { finish(restore) }()
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if s.Resources != nil {
		if err = s.Resources.Release(cleanup, owner, id); err != nil {
			return false, err
		}
	}
	// Resource release may have retired the MCP generation. If SQL later fails,
	// leave the durable session detached so a fresh load must rebind resources.
	restore = false
	s.clearDecisions(owner, id)
	alreadyDeleted, paths, err := s.Store.purgeSession(cleanup, id)
	if err != nil {
		return false, err
	}
	if s.Assets != nil {
		if alreadyDeleted {
			err = s.Assets.CleanupOrphans(cleanup)
		} else {
			err = s.Assets.RemoveOrphans(cleanup, paths)
		}
		if err != nil {
			return alreadyDeleted, err
		}
	}
	return alreadyDeleted, nil
}
