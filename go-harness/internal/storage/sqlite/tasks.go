package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

// TaskStore persists lifecycle snapshots, attempt leases, events and an atomic
// notification outbox. No mutable task state is held in process memory.
type TaskStore struct {
	store         *Store
	activeTimeout time.Duration
	maxValue      int64
	now           func() time.Time
	namespace     string
	hooks         TaskHooks
}

var (
	_ bt.TaskStore          = (*TaskStore)(nil)
	_ bt.TaskEventStore     = (*TaskStore)(nil)
	_ bt.NotificationOutbox = (*TaskStore)(nil)
	_ bt.NotificationWriter = (*TaskStore)(nil)
)

func newTaskStore(store *Store, cfg Config) (*TaskStore, error) {
	s := &TaskStore{store: store, activeTimeout: cfg.ActiveAttemptTimeout, maxValue: cfg.MaxTaskValueBytes, now: time.Now}
	if s.activeTimeout <= 0 {
		s.activeTimeout = 30 * time.Second
	}
	if s.maxValue <= 0 {
		s.maxValue = 1 << 20
	}
	if err := store.db.QueryRow("SELECT value FROM eino_store_identity WHERE id=1").Scan(&s.namespace); err != nil {
		return nil, err
	}
	return s, nil
}

type taskRecord struct {
	task  *bt.Task
	lease int64
}

func loadTask(ctx context.Context, tx *sql.Tx, id string) (*taskRecord, error) {
	var data []byte
	r := &taskRecord{}
	err := tx.QueryRowContext(ctx, "SELECT payload,lease_expires_at FROM eino_background_tasks WHERE id=?", id).Scan(&data, &r.lease)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, bt.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &r.task); err != nil {
		return nil, err
	}
	if r.task == nil || r.task.Spec.ID != id || r.task.Version < 1 {
		return nil, fmt.Errorf("sqlite: corrupt task snapshot %q", id)
	}
	return r, nil
}

func saveTask(ctx context.Context, tx *sql.Tx, r *taskRecord, expected int64) error {
	data, err := json.Marshal(r.task)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE eino_background_tasks SET status=?,version=?,lease_expires_at=?,payload=? WHERE id=? AND version=?`,
		string(r.task.Status), r.task.Version, r.lease, data, r.task.Spec.ID, expected)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return bt.ErrVersionConflict
	}
	return nil
}

func (s *TaskStore) checkSize(name string, data []byte) error {
	if int64(len(data)) > s.maxValue {
		return fmt.Errorf("backgroundtask: %s exceeds configured bounds", name)
	}
	return nil
}

func (s *TaskStore) Create(ctx context.Context, req *bt.CreateTaskRequest) (*bt.Task, error) {
	if req == nil || req.Spec.ID == "" || req.Spec.ExecutorKey == "" {
		return nil, errors.New("backgroundtask: id and executor key are required")
	}
	if req.Spec.NotifySession && req.Spec.SessionID == "" {
		return nil, errors.New("backgroundtask: notification session id is required")
	}
	if req.LeaseExpiryPolicy != bt.LeaseExpiryRetry && req.LeaseExpiryPolicy != bt.LeaseExpiryFail {
		return nil, errors.New("backgroundtask: invalid lease expiry policy")
	}
	for name, data := range map[string][]byte{"checkpoint": req.Checkpoint, "context snapshot": req.ContextSnapshot, "payload": req.Spec.Payload} {
		if err := s.checkSize(name, data); err != nil {
			return nil, err
		}
	}
	now := s.now().UTC()
	task := &bt.Task{Spec: req.Spec, LeaseExpiryPolicy: req.LeaseExpiryPolicy, Status: bt.StatusPending, Checkpoint: req.Checkpoint, ContextSnapshot: req.ContextSnapshot, Version: 1, CreatedAt: now, UpdatedAt: now}
	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO eino_background_tasks(id,executor_key,status,version,payload) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, task.Spec.ID, task.Spec.ExecutorKey, string(task.Status), task.Version, data)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, bt.ErrAlreadyExists
	}
	if s.hooks.Create != nil {
		if err = s.hooks.Create(ctx, tx, cloneTaskSnapshot(task)); err != nil {
			return nil, err
		}
	}
	if err = s.enqueueLifecycle(ctx, tx, task, bt.NotificationTaskCreated); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	// The returned task must not alias the caller's request either.
	var out bt.Task
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *TaskStore) advance(t *bt.Task) { t.Version++; t.UpdatedAt = s.now().UTC() }

func terminal(status bt.Status) bool {
	return status == bt.StatusCompleted || status == bt.StatusFailed || status == bt.StatusCanceled
}

func (s *TaskStore) finish(r *taskRecord) {
	r.lease = 0
	r.task.PendingResume = nil
	s.advance(r.task)
	now := s.now().UTC()
	r.task.DoneAt = &now
}

func statusNotification(status bt.Status) bt.NotificationKind {
	switch status {
	case bt.StatusWaitingInput:
		return bt.NotificationWaitingInput
	case bt.StatusCompleted:
		return bt.NotificationCompleted
	case bt.StatusFailed:
		return bt.NotificationFailed
	case bt.StatusCanceled:
		return bt.NotificationCanceled
	default:
		return ""
	}
}

func (s *TaskStore) expire(r *taskRecord) bool {
	t := r.task
	if t.Status != bt.StatusRunning || r.lease > s.now().UnixNano() {
		return false
	}
	r.lease = 0
	if t.LeaseExpiryPolicy == bt.LeaseExpiryRetry {
		t.Status = bt.StatusPending
		s.advance(t)
		return true
	}
	if t.CancelRequestedAt != nil {
		t.Status = bt.StatusCanceled
		t.ResultError = cancelReason(t.CancelReason)
	} else {
		t.Status = bt.StatusFailed
		t.ResultError = "execution lease expired and retry is disabled"
	}
	s.finish(r)
	return true
}

func cancelReason(reason string) string {
	if reason == "" {
		return "task was canceled"
	}
	return reason
}

// withTask resolves expired leases and commits that recovery even when the
// caller's stale version is subsequently rejected. Transition callbacks must
// validate before mutating, and their failed mutations are never persisted.
func (s *TaskStore) withTask(ctx context.Context, id string, resolve bool, fn func(*sql.Tx, *taskRecord) error) (*bt.Task, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := loadTask(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	before := r.task.Version
	previous := cloneTaskSnapshot(r.task)
	if resolve && s.expire(r) {
		if err = s.transitionHook(ctx, tx, previous, r.task); err != nil {
			return nil, err
		}
		if err = saveTask(ctx, tx, r, before); err != nil {
			return nil, err
		}
		if err = s.enqueueLifecycle(ctx, tx, r.task, statusNotification(r.task.Status)); err != nil {
			return nil, err
		}
	}
	before = r.task.Version
	previous = cloneTaskSnapshot(r.task)
	semanticErr := fn(tx, r)
	if semanticErr != nil && !taskSemanticError(semanticErr) {
		return nil, semanticErr
	}
	if semanticErr == nil && r.task.Version != before {
		if err = s.transitionHook(ctx, tx, previous, r.task); err != nil {
			return nil, err
		}
		if err = saveTask(ctx, tx, r, before); err != nil {
			return nil, err
		}
		if err = s.enqueueLifecycle(ctx, tx, r.task, statusNotification(r.task.Status)); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if semanticErr != nil {
		return nil, semanticErr
	}
	return r.task, nil
}

func taskSemanticError(err error) bool {
	for _, sentinel := range []error{bt.ErrNotFound, bt.ErrVersionConflict, bt.ErrLeaseLost, bt.ErrIllegalTransition, bt.ErrAlreadyTerminal, bt.ErrTaskEventIDConflict, bt.ErrNotificationEventIDConflict, bt.ErrNotificationUnavailable} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

func (s *TaskStore) Get(ctx context.Context, id string) (*bt.Task, error) {
	return s.withTask(ctx, id, true, func(*sql.Tx, *taskRecord) error { return nil })
}

func checkVersion(r *taskRecord, version int64) error {
	if r.task.Version != version {
		return bt.ErrVersionConflict
	}
	return nil
}

func (s *TaskStore) checkActive(r *taskRecord, version int64, allowCancel bool) error {
	if err := checkVersion(r, version); err != nil {
		return err
	}
	if r.task.Status != bt.StatusRunning || r.lease <= s.now().UnixNano() || (!allowCancel && r.task.CancelRequestedAt != nil) {
		return bt.ErrLeaseLost
	}
	return nil
}

func (s *TaskStore) Start(ctx context.Context, req *bt.StartTaskRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: start request is required")
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := checkVersion(r, req.ExpectedVersion); err != nil {
			return err
		}
		if r.task.Status != bt.StatusPending {
			return bt.ErrIllegalTransition
		}
		r.task.Status = bt.StatusRunning
		r.task.Attempt++
		r.lease = s.now().Add(s.activeTimeout).UnixNano()
		s.advance(r.task)
		return nil
	})
}

func (s *TaskStore) Heartbeat(ctx context.Context, req *bt.HeartbeatRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: heartbeat request is required")
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := s.checkActive(r, req.ExpectedVersion, false); err != nil {
			return err
		}
		r.lease = s.now().Add(s.activeTimeout).UnixNano()
		s.advance(r.task)
		return nil
	})
}

func (s *TaskStore) CommitStart(ctx context.Context, req *bt.CommitStartRequest) (*bt.Task, error) {
	if req == nil || len(req.Checkpoint) == 0 {
		return nil, errors.New("backgroundtask: initial checkpoint is required")
	}
	if err := s.checkSize("checkpoint", req.Checkpoint); err != nil {
		return nil, err
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := s.checkActive(r, req.ExpectedVersion, false); err != nil {
			return err
		}
		if len(r.task.Checkpoint) > 0 {
			return bt.ErrIllegalTransition
		}
		r.task.Checkpoint = bytes.Clone(req.Checkpoint)
		s.advance(r.task)
		return nil
	})
}

func (s *TaskStore) ReportTranscriptFailure(ctx context.Context, req *bt.ReportTranscriptFailureRequest) (*bt.Task, error) {
	if req == nil || req.Error == "" || len(req.Error) > 4096 {
		return nil, errors.New("backgroundtask: transcript error must be 1 to 4096 bytes")
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := s.checkActive(r, req.ExpectedVersion, true); err != nil {
			return err
		}
		if r.task.OutputFileErr == "" {
			r.task.OutputFileErr = req.Error
			s.advance(r.task)
		}
		return nil
	})
}

func (s *TaskStore) Complete(ctx context.Context, req *bt.CompleteTaskRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: complete request is required")
	}
	if err := s.checkSize("result", req.Data); err != nil {
		return nil, err
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := s.checkActive(r, req.ExpectedVersion, false); err != nil {
			return err
		}
		r.task.Status = bt.StatusCompleted
		r.task.ResultData = bytes.Clone(req.Data)
		r.task.ResultError = ""
		s.finish(r)
		return nil
	})
}

func (s *TaskStore) Fail(ctx context.Context, req *bt.FailTaskRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: fail request is required")
	}
	if req.Error == "" {
		return nil, fmt.Errorf("%w: failure requires an error", bt.ErrInvalidExecutionResult)
	}
	if len(req.Error) > 4096 {
		return nil, errors.New("backgroundtask: failure exceeds 4096 bytes")
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := s.checkActive(r, req.ExpectedVersion, false); err != nil {
			return err
		}
		r.task.Status = bt.StatusFailed
		r.task.ResultData = nil
		r.task.ResultError = req.Error
		s.finish(r)
		return nil
	})
}

func (s *TaskStore) pause(ctx context.Context, id string, version int64, checkpoint []byte, status bt.Status) (*bt.Task, error) {
	if len(checkpoint) == 0 && status != bt.StatusPending {
		return nil, errors.New("backgroundtask: checkpointed pause requires checkpoint data")
	}
	if err := s.checkSize("checkpoint", checkpoint); err != nil {
		return nil, err
	}
	return s.withTask(ctx, id, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := s.checkActive(r, version, false); err != nil {
			return err
		}
		r.task.Status = status
		if len(checkpoint) > 0 {
			r.task.Checkpoint = bytes.Clone(checkpoint)
		}
		if status != bt.StatusPending {
			r.task.PendingResume = nil
		}
		r.lease = 0
		s.advance(r.task)
		return nil
	})
}

func (s *TaskStore) WaitInput(ctx context.Context, req *bt.WaitInputTaskRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: wait input request is required")
	}
	return s.pause(ctx, req.TaskID, req.ExpectedVersion, req.Checkpoint, bt.StatusWaitingInput)
}
func (s *TaskStore) Suspend(ctx context.Context, req *bt.SuspendTaskRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: suspend request is required")
	}
	return s.pause(ctx, req.TaskID, req.ExpectedVersion, req.Checkpoint, bt.StatusSuspended)
}
func (s *TaskStore) Yield(ctx context.Context, req *bt.YieldTaskRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: yield request is required")
	}
	return s.pause(ctx, req.TaskID, req.ExpectedVersion, req.Checkpoint, bt.StatusPending)
}

func (s *TaskStore) AckCancel(ctx context.Context, req *bt.AckCancelRequest) (*bt.Task, error) {
	if req == nil || len(req.Reason) > 4096 {
		return nil, errors.New("backgroundtask: cancellation request is required and reason cannot exceed 4096 bytes")
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := s.checkActive(r, req.ExpectedVersion, true); err != nil {
			return err
		}
		reason := r.task.CancelReason
		if reason == "" {
			reason = req.Reason
		}
		r.task.Status = bt.StatusCanceled
		r.task.ResultData = nil
		r.task.ResultError = cancelReason(reason)
		s.finish(r)
		return nil
	})
}

func (s *TaskStore) RequestCancel(ctx context.Context, req *bt.RequestCancelRequest) (*bt.Task, error) {
	if req == nil || len(req.Reason) > 4096 {
		return nil, errors.New("backgroundtask: cancellation request is required and reason cannot exceed 4096 bytes")
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := checkVersion(r, req.ExpectedVersion); err != nil {
			return err
		}
		if terminal(r.task.Status) {
			return bt.ErrAlreadyTerminal
		}
		if r.task.CancelRequestedAt != nil {
			return nil
		}
		now := s.now().UTC()
		r.task.CancelRequestedAt = &now
		r.task.CancelReason = req.Reason
		if r.task.Status == bt.StatusRunning {
			r.lease = now.Add(s.activeTimeout).UnixNano()
			s.advance(r.task)
		} else if r.task.Status == bt.StatusPending && r.task.LeaseExpiryPolicy == bt.LeaseExpiryRetry && r.task.Attempt > 0 {
			s.advance(r.task)
		} else {
			r.task.Status = bt.StatusCanceled
			r.task.ResultError = cancelReason(req.Reason)
			s.finish(r)
		}
		return nil
	})
}

func (s *TaskStore) Resume(ctx context.Context, req *bt.ResumeRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: resume request is required")
	}
	if err := s.checkSize("resume data", req.Data); err != nil {
		return nil, err
	}
	if err := s.checkSize("context snapshot", req.ContextSnapshot); err != nil {
		return nil, err
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := checkVersion(r, req.ExpectedVersion); err != nil {
			return err
		}
		if r.task.Status != bt.StatusWaitingInput {
			return bt.ErrIllegalTransition
		}
		r.task.PendingResume = bytes.Clone(req.Data)
		if req.ContextSnapshot != nil {
			r.task.ContextSnapshot = append([]byte{}, req.ContextSnapshot...)
		}
		r.task.Status = bt.StatusPending
		s.advance(r.task)
		return nil
	})
}

func (s *TaskStore) ReleaseSuspension(ctx context.Context, req *bt.ReleaseSuspensionRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: release request is required")
	}
	if err := s.checkSize("context snapshot", req.ContextSnapshot); err != nil {
		return nil, err
	}
	return s.withTask(ctx, req.TaskID, true, func(_ *sql.Tx, r *taskRecord) error {
		if err := checkVersion(r, req.ExpectedVersion); err != nil {
			return err
		}
		if r.task.Status != bt.StatusSuspended {
			return bt.ErrIllegalTransition
		}
		if req.ContextSnapshot != nil {
			r.task.ContextSnapshot = append([]byte{}, req.ContextSnapshot...)
		}
		r.task.Status = bt.StatusPending
		s.advance(r.task)
		return nil
	})
}

func (s *TaskStore) WaitForTaskVersion(ctx context.Context, req *bt.WaitForTaskVersionRequest) (*bt.Task, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: version wait request is required")
	}
	// Polling reads authoritative SQLite state, including updates from another
	// connection or a restarted daemon. It also resolves leases without relying
	// on a process-local writer notification.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := s.Get(ctx, req.TaskID)
		if err != nil {
			return nil, err
		}
		if task.Version > req.AfterVersion {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
