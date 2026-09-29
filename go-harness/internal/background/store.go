package background

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Service) migrate(ctx context.Context) error {
	_, err := s.store.DB().ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_background_bindings (
 task_id TEXT PRIMARY KEY REFERENCES eino_background_tasks(id),
 parent_session_id TEXT NOT NULL, child_session_id TEXT NOT NULL,
 origin_run_id TEXT NOT NULL, origin_tool_call_id TEXT NOT NULL,
 intent_hash TEXT NOT NULL, payload BLOB NOT NULL, blocked_reason TEXT NOT NULL DEFAULT '',
 UNIQUE(parent_session_id,origin_run_id,origin_tool_call_id)
);
CREATE INDEX IF NOT EXISTS harness_background_parent ON harness_background_bindings(parent_session_id,task_id);
CREATE TABLE IF NOT EXISTS harness_background_child_leases (
 child_session_id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES eino_background_tasks(id), attempt INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS harness_background_execution_failures (
 task_id TEXT NOT NULL REFERENCES eino_background_tasks(id),attempt INTEGER NOT NULL,
 error TEXT NOT NULL,persistence_failure INTEGER NOT NULL,PRIMARY KEY(task_id,attempt)
);
CREATE TABLE IF NOT EXISTS harness_background_inbox (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, notification_id TEXT UNIQUE NOT NULL,
 parent_session_id TEXT NOT NULL, task_id TEXT NOT NULL REFERENCES eino_background_tasks(id),
 payload BLOB NOT NULL, handled INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS harness_background_inbox_parent ON harness_background_inbox(parent_session_id,handled,seq);`)
	return err
}

type queryRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadBinding(ctx context.Context, db queryRow, taskID string) (Binding, string, error) {
	var data []byte
	var blocked string
	err := db.QueryRowContext(ctx, "SELECT payload,blocked_reason FROM harness_background_bindings WHERE task_id=?", taskID).Scan(&data, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, "", harness.ErrNotFound
	}
	if err != nil {
		return Binding{}, "", err
	}
	var binding Binding
	if err = json.Unmarshal(data, &binding); err != nil {
		return Binding{}, "", err
	}
	if binding.TaskID != taskID || binding.ParentSessionID == "" {
		return Binding{}, "", errors.New("corrupt background binding")
	}
	return binding, blocked, nil
}

func originTaskID(b Binding) string {
	data, _ := json.Marshal([]string{b.ParentSessionID, b.OriginRunID, b.OriginToolCallID})
	return fmt.Sprintf("harness-bg-%x", sha256.Sum256(data))
}

func prepareSubmission(actor harness.TaskActor, in Submission) (Submission, error) {
	b := in.Binding
	if actor.OwnerID == "" || actor.SessionID == "" || b.ParentSessionID != actor.SessionID || b.OriginRunID == "" || b.OriginToolCallID == "" || b.Workspace == "" || b.ExecutionContract == "" || b.RootBudgetID == "" || b.ConfigVersion < 1 || in.Spec.ExecutorKey == "" {
		return Submission{}, fmt.Errorf("%w: incomplete background submission", harness.ErrInvalidInput)
	}
	for _, value := range []string{b.ParentSessionID, b.OriginRunID, b.OriginToolCallID, b.ChildSessionID, b.AgentVersion, b.ExecutionContract, b.RootBudgetID} {
		if len(value) > 4096 || strings.ContainsRune(value, 0) {
			return Submission{}, harness.ErrInvalidInput
		}
	}
	b.TaskID = originTaskID(b)
	b.SubmittedBy = actor.OwnerID
	b.ExecutorKey = in.Spec.ExecutorKey
	if b.ChildSessionID == "" {
		b.ChildSessionID = b.TaskID + "/session"
	}
	if b.ChildSessionID == b.ParentSessionID {
		return Submission{}, harness.ErrPermissionDenied
	}
	in.Spec.ID, in.Spec.SessionID = b.TaskID, b.ParentSessionID
	in.Spec.Payload = append([]byte(nil), in.Spec.Payload...)
	b.IntentHash = ""
	// A reconnect may change SubmittedBy without changing accepted intent.
	identity := b
	identity.SubmittedBy = ""
	data, err := json.Marshal(struct {
		Binding Binding
		Spec    bt.Spec
	}{identity, in.Spec})
	if err != nil {
		return Submission{}, err
	}
	b.IntentHash = fmt.Sprintf("%x", sha256.Sum256(data))
	in.Binding = b
	return in, nil
}

func (s *Service) authorize(ctx context.Context, actor harness.TaskActor) error {
	if actor.OwnerID == "" || actor.SessionID == "" || s.config.Authorizer == nil {
		return harness.ErrPermissionDenied
	}
	return s.config.Authorizer.AuthorizeTaskAccess(ctx, actor)
}

func (s *Service) authorizedBinding(ctx context.Context, actor harness.TaskActor, id string) (Binding, string, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return Binding{}, "", err
	}
	b, blocked, err := loadBinding(ctx, s.store.DB(), id)
	if err != nil {
		return Binding{}, "", err
	}
	if b.ParentSessionID != actor.SessionID {
		return Binding{}, "", harness.ErrNotFound
	}
	return b, blocked, nil
}

func project(task *bt.Task, b Binding, blocked string) harness.BackgroundTask {
	return harness.BackgroundTask{ID: task.Spec.ID, SessionID: b.ParentSessionID, ChildSessionID: b.ChildSessionID, Description: task.Spec.Description, Status: string(task.Status), Version: task.Version, Attempt: task.Attempt, Result: append([]byte(nil), task.ResultData...), Error: task.ResultError, BlockedReason: blocked, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt}
}

func (s *Service) Get(ctx context.Context, actor harness.TaskActor, id string) (harness.BackgroundTask, error) {
	b, blocked, err := s.authorizedBinding(ctx, actor, id)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	task, err := s.manager.Get(ctx, id)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	result := project(task, b, blocked)
	if err = s.projectExecutionFailure(ctx, &result); err != nil {
		return harness.BackgroundTask{}, err
	}
	return result, nil
}

// BindingForTask is a host-only lookup used to enrich an authorized task with
// broker state. It repeats attachment checks and never accepts caller bindings.
func (s *Service) BindingForTask(ctx context.Context, actor harness.TaskActor, id string) (Binding, error) {
	binding, _, err := s.authorizedBinding(ctx, actor, id)
	return binding, err
}

// List uses a task-ID keyset cursor scoped by the authenticated parent session.
func (s *Service) List(ctx context.Context, actor harness.TaskActor, after string, limit int) ([]harness.BackgroundTask, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.store.DB().QueryContext(ctx, "SELECT task_id FROM harness_background_bindings WHERE parent_session_id=? AND task_id>? ORDER BY task_id LIMIT ?", actor.SessionID, after, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]harness.BackgroundTask, 0, len(ids))
	for _, id := range ids {
		task, err := s.Get(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, nil
}

func (s *Service) Wait(ctx context.Context, actor harness.TaskActor, id string, after int64) (harness.BackgroundTask, error) {
	if _, _, err := s.authorizedBinding(ctx, actor, id); err != nil {
		return harness.BackgroundTask{}, err
	}
	if _, err := s.manager.WaitForTaskVersion(ctx, &bt.WaitForTaskVersionRequest{TaskID: id, AfterVersion: after}); err != nil {
		return harness.BackgroundTask{}, err
	}
	return s.Get(ctx, actor, id) // revalidate an attachment that may have changed while waiting
}

func (s *Service) Cancel(ctx context.Context, actor harness.TaskActor, id, reason string) (harness.BackgroundTask, error) {
	if _, _, err := s.authorizedBinding(ctx, actor, id); err != nil {
		return harness.BackgroundTask{}, err
	}
	if _, err := s.manager.RequestCancel(ctx, id, bt.WithCancellationReason(reason)); err != nil {
		return harness.BackgroundTask{}, err
	}
	s.wake()
	return s.Get(ctx, actor, id)
}

func (s *Service) CheckEffect(ctx context.Context) error {
	scope, ok := ScopeFromContext(ctx)
	if !ok {
		return harness.ErrPermissionDenied
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.CheckEffectTx(ctx, tx, scope); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckEffectTx fences a budget reservation/dispatch in its own transaction.
// It must be called immediately before committing durable permission for I/O;
// a check in a separate transaction cannot protect the reservation write.
func (s *Service) CheckEffectTx(ctx context.Context, tx *sql.Tx, scope TaskScope) error {
	if err := s.checkChildTx(ctx, tx, scope, false); err != nil {
		return err
	}
	s.mu.Lock()
	state := s.attempts[attemptKey(scope)]
	s.mu.Unlock()
	if state == nil {
		return bt.ErrLeaseLost
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ready {
		return bt.ErrLeaseLost
	}
	return nil
}

func (s *Service) checkChildTx(ctx context.Context, tx *sql.Tx, scope TaskScope, allowStopping bool) error {
	if err := s.tasks.CheckAttemptTx(ctx, tx, scope.Binding.TaskID, scope.Attempt, allowStopping); err != nil {
		return err
	}
	if !allowStopping {
		_, blocked, err := loadBinding(ctx, tx, scope.Binding.TaskID)
		if err != nil {
			return err
		}
		if blocked != "" {
			return harness.ErrBackgroundUncertain
		}
	}
	var taskID string
	var attempt int64
	err := tx.QueryRowContext(ctx, "SELECT task_id,attempt FROM harness_background_child_leases WHERE child_session_id=?", scope.Binding.ChildSessionID).Scan(&taskID, &attempt)
	if err != nil {
		return bt.ErrLeaseLost
	}
	if taskID != scope.Binding.TaskID || attempt != scope.Attempt {
		return bt.ErrLeaseLost
	}
	return nil
}

func (s *Service) acquireChildTx(ctx context.Context, tx *sql.Tx, b Binding, attempt int64) error {
	var uncertain int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM harness_background_bindings WHERE child_session_id=? AND blocked_reason<>''", b.ChildSessionID).Scan(&uncertain); err != nil {
		return err
	}
	if uncertain != 0 {
		return harness.ErrBackgroundUncertain
	}
	var oldID string
	var oldAttempt int64
	err := tx.QueryRowContext(ctx, "SELECT task_id,attempt FROM harness_background_child_leases WHERE child_session_id=?", b.ChildSessionID).Scan(&oldID, &oldAttempt)
	if err == nil {
		old, expires, loadErr := s.tasks.TaskLeaseTx(ctx, tx, oldID)
		if loadErr != nil {
			return loadErr
		}
		if old.Status == bt.StatusRunning && old.Attempt == oldAttempt && expires.After(time.Now()) {
			return harness.ErrChildSessionBusy
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	s.mu.Lock()
	for _, state := range s.attempts {
		if state.scope.Binding.ChildSessionID == b.ChildSessionID && !state.isJoined() {
			s.mu.Unlock()
			return harness.ErrChildSessionBusy
		}
	}
	s.mu.Unlock()
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_child_leases(child_session_id,task_id,attempt) VALUES(?,?,?) ON CONFLICT(child_session_id) DO UPDATE SET task_id=excluded.task_id,attempt=excluded.attempt`, b.ChildSessionID, b.TaskID, attempt)
	return err
}
