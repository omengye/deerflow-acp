package background

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

// UNION makes reads terminate even if a damaged database contains a cycle.
const descendantCTE = `WITH RECURSIVE descendants(task_id) AS (
 SELECT task_id FROM harness_background_tree WHERE task_id=?
 UNION SELECT t.task_id FROM harness_background_tree t JOIN descendants d ON t.parent_task_id=d.task_id
 UNION SELECT l.task_id FROM harness_background_continuation_links l JOIN descendants d ON l.previous_task_id=d.task_id
)`

func migrateTreeTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_background_tree (
 task_id TEXT PRIMARY KEY REFERENCES harness_background_bindings(task_id),
 root_session_id TEXT NOT NULL, parent_task_id TEXT NOT NULL,
 depth INTEGER NOT NULL CHECK(depth BETWEEN 1 AND 3)
);
CREATE INDEX IF NOT EXISTS harness_background_tree_root ON harness_background_tree(root_session_id,task_id);
CREATE INDEX IF NOT EXISTS harness_background_tree_parent ON harness_background_tree(parent_task_id,task_id);
CREATE TABLE IF NOT EXISTS harness_background_continuation_links (
 task_id TEXT PRIMARY KEY REFERENCES harness_background_bindings(task_id),
 previous_task_id TEXT NOT NULL REFERENCES harness_background_bindings(task_id)
);
CREATE INDEX IF NOT EXISTS harness_background_continuation_previous ON harness_background_continuation_links(previous_task_id);
CREATE TABLE IF NOT EXISTS harness_background_stops (
 task_id TEXT PRIMARY KEY REFERENCES harness_background_bindings(task_id),
 reason TEXT NOT NULL,created_at TEXT NOT NULL
);`); err != nil {
		return err
	}
	// Backfill projections without rewriting legacy binding bytes or hashes.
	// Pages are closed before writing: SQLite may have only one connection.
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT b.payload FROM harness_background_bindings b LEFT JOIN harness_background_tree t ON t.task_id=b.task_id WHERE t.task_id IS NULL AND b.task_id>? ORDER BY b.task_id LIMIT 256`, after)
		if err != nil {
			return err
		}
		var bindings []Binding
		for rows.Next() {
			var data []byte
			var b Binding
			if err = rows.Scan(&data); err == nil {
				err = json.Unmarshal(data, &b)
			}
			if err != nil {
				break
			}
			bindings = append(bindings, b)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, b := range bindings {
			if err = insertTreeTx(ctx, tx, b); err != nil {
				return err
			}
			after = b.TaskID
		}
		if len(bindings) < 256 {
			return nil
		}
	}
}

func insertTreeTx(ctx context.Context, tx *sql.Tx, b Binding) error {
	if b.RootSession() == "" || b.TaskID == "" {
		return harness.ErrInvalidInput
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO harness_background_tree(task_id,root_session_id,parent_task_id,depth) VALUES(?,?,?,?)`, b.TaskID, b.RootSession(), b.ParentTaskID, b.TaskDepth())
	return err
}

// WorkerActor is a label derived from an active, host-owned attempt context.
// Copying this label into a new context grants no authority.
func (s *Service) WorkerActor(ctx context.Context) (harness.TaskActor, error) {
	scope, ok := ScopeFromContext(ctx)
	if !ok {
		return harness.TaskActor{}, harness.ErrPermissionDenied
	}
	if err := s.CheckEffect(ctx); err != nil {
		return harness.TaskActor{}, err
	}
	return harness.TaskActor{OwnerID: "background/" + attemptKey(scope), SessionID: scope.Binding.ChildSessionID}, nil
}

func (s *Service) bindSubmissionTree(ctx context.Context, tx *sql.Tx, b Binding) (Binding, error) {
	depth := 0
	if b.ParentInvocationID != "" {
		if s.config.RolePlacement == nil {
			return Binding{}, harness.ErrPermissionDenied
		}
		role, err := s.config.RolePlacement(ctx, tx)
		if err != nil {
			return Binding{}, err
		}
		if role == nil || role.ID != b.ParentInvocationID || role.RootBudgetID != b.RootBudgetID || role.Depth < 1 || role.Depth >= 3 {
			return Binding{}, harness.ErrPermissionDenied
		}
		depth = role.Depth + 1
		if b.Depth != 0 && b.Depth != depth {
			return Binding{}, harness.ErrPermissionDenied
		}
	}
	scope, worker := ScopeFromContext(ctx)
	if !worker {
		if b.RootSessionID != "" || b.ParentTaskID != "" || (depth == 0 && b.Depth != 0) {
			return Binding{}, harness.ErrPermissionDenied
		}
		b.Depth = depth
		return b, nil
	}
	p := scope.Binding
	if b.ParentSessionID != p.ChildSessionID || b.RootBudgetID != p.RootBudgetID || b.Workspace != p.Workspace || p.TaskDepth() >= 3 {
		return Binding{}, harness.ErrPermissionDenied
	}
	if depth == 0 {
		depth = p.TaskDepth() + 1
	}
	if depth <= p.TaskDepth() || depth > 3 || (b.ParentTaskID != "" && b.ParentTaskID != p.TaskID) ||
		(b.RootSessionID != "" && b.RootSessionID != p.RootSession()) ||
		(b.Depth != 0 && b.Depth != depth) {
		return Binding{}, harness.ErrPermissionDenied
	}
	b.RootSessionID, b.ParentTaskID, b.Depth = p.RootSession(), p.TaskID, depth
	return b, nil
}

// Repeat ancestry and lease validation in the same transaction that accepts
// the child. Cancellation cannot race this check and admit an unmarked child.
func (s *Service) checkSubmissionTreeTx(ctx context.Context, tx *sql.Tx, b Binding) error {
	if err := agents.CheckStoppedIfConfigured(ctx, tx, b.RootBudgetID, b.ParentInvocationID); err != nil {
		return err
	}
	expected, err := s.bindSubmissionTree(ctx, tx, b)
	if err != nil {
		return err
	}
	if expected != b {
		return harness.ErrPermissionDenied
	}
	if scope, ok := ScopeFromContext(ctx); ok {
		return s.CheckEffectTx(ctx, tx, scope)
	}
	var child bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_bindings WHERE child_session_id=?)`, b.ParentSessionID).Scan(&child); err != nil {
		return err
	}
	if child {
		return harness.ErrPermissionDenied
	}
	return nil
}

func (s *Service) canAccessBinding(ctx context.Context, db queryRow, actor harness.TaskActor, b Binding) (bool, error) {
	if scope, ok := ScopeFromContext(ctx); ok {
		if b.RootSession() != scope.Binding.RootSession() || b.TaskID == scope.Binding.TaskID {
			return false, nil
		}
		if scope.Binding.ContinuationOf != "" {
			if b.RootBudgetID != scope.Binding.RootBudgetID || b.RoleID() == scope.Binding.RoleID() {
				return false, nil
			}
			return agents.IsDescendant(ctx, db, scope.Binding.RootBudgetID, scope.Binding.RoleID(), b.RoleID())
		}
		var descendant bool
		err := db.QueryRowContext(ctx, descendantCTE+` SELECT EXISTS(SELECT 1 FROM descendants WHERE task_id=?)`, scope.Binding.TaskID, b.TaskID).Scan(&descendant)
		return descendant, err
	}
	return b.RootSession() == actor.SessionID, nil
}

// TaskRoleID reads host-owned logical identity. The caller still checks root,
// actor and ancestry; knowing this string alone never grants task access.
func TaskRoleID(ctx context.Context, db queryRow, id string) (string, error) {
	b, _, err := loadBinding(ctx, db, id)
	if err != nil {
		return "", err
	}
	return b.RoleID(), nil
}

func OwnsTask(ctx context.Context, db queryRow, owner Binding, id string) (bool, error) {
	b, _, err := loadBinding(ctx, db, id)
	if err != nil {
		return false, err
	}
	if b.TaskID == owner.TaskID || b.RootSession() != owner.RootSession() || b.RootBudgetID != owner.RootBudgetID {
		return false, nil
	}
	if owner.ContinuationOf != "" {
		if b.RoleID() == owner.RoleID() {
			return false, nil
		}
		return agents.IsDescendant(ctx, db, owner.RootBudgetID, owner.RoleID(), b.RoleID())
	}
	var allowed bool
	err = db.QueryRowContext(ctx, descendantCTE+` SELECT EXISTS(SELECT 1 FROM descendants WHERE task_id=?)`, owner.TaskID, id).Scan(&allowed)
	return allowed, err
}

func checkTreeRunning(ctx context.Context, db queryRow, id string) error {
	var stopped bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_stops WHERE task_id=?)`, id).Scan(&stopped); err != nil {
		return err
	}
	if stopped {
		return bt.ErrLeaseLost
	}
	return nil
}

func (s *Service) cancelSubtree(ctx context.Context, actor harness.TaskActor, id, reason string) (harness.BackgroundTask, error) {
	if len(reason) > 4096 {
		return harness.BackgroundTask{}, harness.ErrInvalidInput
	}
	if _, _, err := s.authorizedBinding(ctx, actor, id); err != nil {
		return harness.BackgroundTask{}, err
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	defer tx.Rollback()
	if scope, ok := ScopeFromContext(ctx); ok {
		if err = s.CheckEffectTx(ctx, tx, scope); err != nil {
			return harness.BackgroundTask{}, err
		}
	}
	b, _, err := loadBinding(ctx, tx, id)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	allowed, err := s.canAccessBinding(ctx, tx, actor, b)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	if !allowed {
		return harness.BackgroundTask{}, harness.ErrNotFound
	}
	_, err = tx.ExecContext(ctx, descendantCTE+`
 INSERT INTO harness_background_stops(task_id,reason,created_at)
 SELECT task_id,?,? FROM descendants WHERE true ON CONFLICT(task_id) DO NOTHING`, id, reason, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	if err = tx.Commit(); err != nil {
		return harness.BackgroundTask{}, err
	}
	s.wake()
	// The durable fence already prevents new effects and descendants. Native
	// cancellation may still be joining I/O; scans retry after transient faults.
	if err = s.applyStops(ctx, id); err != nil {
		return harness.BackgroundTask{}, err
	}
	return s.Get(ctx, actor, id)
}

func (s *Service) applyStops(ctx context.Context, subtree string) error {
	after := ""
	var failures []error
	for {
		query := `SELECT s.task_id,s.reason FROM harness_background_stops s JOIN eino_background_tasks t ON t.id=s.task_id WHERE t.status NOT IN ('completed','failed','canceled') AND s.task_id>? ORDER BY s.task_id LIMIT 100`
		args := []any{after}
		if subtree != "" {
			query = descendantCTE + ` SELECT s.task_id,s.reason FROM harness_background_stops s JOIN descendants d ON d.task_id=s.task_id JOIN eino_background_tasks t ON t.id=s.task_id WHERE t.status NOT IN ('completed','failed','canceled') AND s.task_id>? ORDER BY s.task_id LIMIT 100`
			args = []any{subtree, after}
		}
		rows, err := s.store.DB().QueryContext(ctx, query, args...)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		type stop struct{ id, reason string }
		var batch []stop
		for rows.Next() {
			var item stop
			if err = rows.Scan(&item.id, &item.reason); err != nil {
				break
			}
			batch = append(batch, item)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		for _, item := range batch {
			_, err = s.manager.RequestCancel(ctx, item.id, bt.WithCancellationReason(item.reason))
			if err != nil && !errors.Is(err, bt.ErrAlreadyTerminal) {
				failures = append(failures, fmt.Errorf("cancel task %s: %w", item.id, err))
			}
			after = item.id
		}
		if len(batch) < 100 {
			return errors.Join(failures...)
		}
	}
}
