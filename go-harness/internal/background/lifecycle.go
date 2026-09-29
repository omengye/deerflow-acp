package background

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	ds "github.com/cloudwego/eino/adk/backgroundtask/subagent"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type checkpointWrite struct {
	Data   []byte
	Delete bool
}
type attemptState struct {
	mu                    sync.Mutex
	scope                 TaskScope
	writes                map[string]checkpointWrite
	joined, ready, failed bool
	cleanupErr            error
}

func (a *attemptState) isJoined() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.joined }

func (s *Service) onCreate(ctx context.Context, tx *sql.Tx, task *bt.Task) error {
	b, ok := ctx.Value(createContextKey{}).(Binding)
	if !ok {
		request, has := ctx.Value(nativeContextKey{}).(*nativeSubmission)
		if !has {
			return errors.New("background: task creation lacks an authorized binding")
		}
		prepared, err := prepareSubmission(request.actor, Submission{Binding: request.binding, Spec: task.Spec})
		if err != nil {
			return err
		}
		b = prepared.Binding
	}
	if b.TaskID != task.Spec.ID || b.ExecutorKey != task.Spec.ExecutorKey || b.ParentSessionID != task.Spec.SessionID {
		return harness.ErrInvalidInput
	}
	if s.config.Budgets == nil {
		return harness.ErrBackgroundUnavailable
	}
	// Child-session continuation is restricted to one immutable parent/workspace
	// contract. Matching a public child ID alone confers no authority.
	var previous []byte
	err := tx.QueryRowContext(ctx, "SELECT payload FROM harness_background_bindings WHERE child_session_id=? LIMIT 1", b.ChildSessionID).Scan(&previous)
	if err == nil {
		var old Binding
		if err = json.Unmarshal(previous, &old); err != nil {
			return err
		}
		if old.ParentSessionID != b.ParentSessionID || old.Workspace != b.Workspace || old.ExecutionContract != b.ExecutionContract || old.AgentVersion != b.AgentVersion {
			return harness.ErrPermissionDenied
		}
	} else {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Only the service-generated namespace may create a new child. An
		// arbitrary existing session ID must never become writable by guessing.
		if b.ChildSessionID != b.TaskID+"/session" {
			return harness.ErrPermissionDenied
		}
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_bindings(task_id,parent_session_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload) VALUES(?,?,?,?,?,?,?)`, b.TaskID, b.ParentSessionID, b.ChildSessionID, b.OriginRunID, b.OriginToolCallID, b.IntentHash, data)
	if err != nil {
		return err
	}
	return s.config.Budgets.BindTaskTx(ctx, tx, b)
}

func (s *Service) onTransition(ctx context.Context, tx *sql.Tx, before, after *bt.Task) error {
	b, blocked, err := loadBinding(ctx, tx, after.Spec.ID)
	if err != nil {
		return err
	}
	if before.Status == bt.StatusPending && after.Status == bt.StatusRunning {
		if blocked != "" {
			return fmt.Errorf("%w: %s", harness.ErrBackgroundUncertain, blocked)
		}
		return s.acquireChildTx(ctx, tx, b, after.Attempt)
	}
	if before.Status == bt.StatusWaitingInput && after.Status == bt.StatusPending {
		resolution, ok := ctx.Value(resumeContextKey{}).(*resumeContext)
		if !ok || resolution.binding.TaskID != b.TaskID || s.config.Approvals == nil {
			return harness.ErrPermissionDenied
		}
		if err = s.config.Approvals.CommitResumeTx(ctx, tx, resolution.actor, b, resolution.grant); err != nil {
			return err
		}
	}
	if before.Status == bt.StatusRunning && after.Status == bt.StatusRunning {
		if ledger, ok := s.config.Budgets.(HeartbeatBudgetLedger); ok {
			return ledger.HeartbeatAttemptTx(ctx, tx, TaskScope{Binding: b, Attempt: before.Attempt})
		}
		return nil
	}
	if before.Status != bt.StatusRunning {
		return nil
	}
	scope := TaskScope{Binding: b, Attempt: before.Attempt}
	s.mu.Lock()
	state := s.attempts[attemptKey(scope)]
	s.mu.Unlock()
	if state == nil && (after.Status == bt.StatusCompleted || after.Status == bt.StatusWaitingInput || after.Status == bt.StatusSuspended) {
		return errors.New("background: checkpoint/complete transition has no joined attempt")
	}
	if state != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		if !state.ready && (after.Status == bt.StatusCanceled || after.Status == bt.StatusFailed) {
			return errors.Join(harness.ErrBackgroundUncertain, errors.New("background: native lease expired before local cleanup joined"))
		}
		if !state.ready && (after.Status == bt.StatusCompleted || after.Status == bt.StatusWaitingInput || after.Status == bt.StatusSuspended) {
			return errors.New("background: transition before attempt cleanup")
		}
		if state.cleanupErr != nil {
			// An expired attempt may become pending natively; quarantine its
			// binding so recovery never repeats uncertain external effects.
			if after.Status == bt.StatusPending {
				_, err = tx.ExecContext(ctx, "UPDATE harness_background_bindings SET blocked_reason=? WHERE task_id=?", state.cleanupErr.Error(), b.TaskID)
				return err
			}
			return errors.Join(harness.ErrBackgroundUncertain, state.cleanupErr)
		}
		if state.ready {
			if !state.joined {
				return errors.New("background: terminal transition before attempt cleanup")
			}
			if s.config.Budgets == nil {
				return harness.ErrBackgroundUnavailable
			}
			if err = s.config.Budgets.CommitAttemptTx(ctx, tx, scope, string(after.Status)); err != nil {
				return err
			}
			if !state.failed && (after.Status == bt.StatusWaitingInput || after.Status == bt.StatusSuspended || after.Status == bt.StatusCompleted) {
				if b.ExecutorKey == ds.ExecutorKey && after.Status != bt.StatusCompleted {
					key := b.TaskID + "/checkpoint"
					write, staged := state.writes[key]
					if staged && (write.Delete || len(write.Data) == 0) {
						return errors.New("background: native pause has no runner checkpoint")
					}
					if !staged {
						var size int
						if err = tx.QueryRowContext(ctx, "SELECT length(payload) FROM eino_checkpoints WHERE id=?", key).Scan(&size); err != nil || size == 0 {
							return errors.New("background: native pause has no runner checkpoint")
						}
					}
				}
				for key, write := range state.writes {
					// Pausing keeps the native runner checkpoint; completion can
					// consume it. Failure/cancellation preserve prior bytes.
					if write.Delete {
						if after.Status == bt.StatusCompleted {
							err = s.store.DeleteCheckpointTx(ctx, tx, key)
						}
					} else if after.Status != bt.StatusCompleted {
						err = s.store.SetCheckpointTx(ctx, tx, key, write.Data)
					}
					if err != nil {
						return err
					}
				}
			}
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM harness_background_child_leases WHERE child_session_id=? AND task_id=? AND attempt=?", b.ChildSessionID, b.TaskID, before.Attempt)
	return err
}

type managedExecutor struct {
	service *Service
	inner   bt.Executor
}

func (e *managedExecutor) Key() string { return e.inner.Key() }
func (e *managedExecutor) LeaseExpiryPolicy() bt.LeaseExpiryPolicy {
	return e.inner.LeaseExpiryPolicy()
}
func (e *managedExecutor) ValidateSpec(spec bt.Spec) error { return e.inner.ValidateSpec(spec) }
func (e *managedExecutor) SupportsDrain() bool             { return e.inner.SupportsDrain() }
func (e *managedExecutor) ValidateExecution(ctx context.Context, task *bt.Task) error {
	b, blocked, err := loadBinding(ctx, e.service.store.DB(), task.Spec.ID)
	if err != nil {
		return err
	}
	if blocked != "" {
		return harness.ErrBackgroundUncertain
	}
	if e.service.config.Budgets == nil {
		return harness.ErrBackgroundUnavailable
	}
	if e.service.config.Attempts != nil {
		if err = e.service.config.Attempts.Validate(ctx, b); err != nil {
			return err
		}
	}
	return e.inner.ValidateExecution(ctx, task)
}
func (e *managedExecutor) Execute(ctx context.Context, task *bt.Task, runtime bt.ExecutionRuntime) (result *bt.ExecutionResult, returnErr error) {
	s := e.service
	b, _, err := loadBinding(ctx, s.store.DB(), task.Spec.ID)
	if err != nil {
		return nil, err
	}
	state := &attemptState{scope: TaskScope{Binding: b, Attempt: task.Attempt}, writes: map[string]checkpointWrite{}}
	s.mu.Lock()
	s.attempts[attemptKey(state.scope)] = state
	s.mu.Unlock()
	v := &attemptContext{state: state}
	ctx = context.WithValue(ctx, attemptContextKey{}, v)
	var stopMonitor func() error
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("background executor panic: %v", recovered)
			result = nil
		}
		if stopMonitor != nil {
			if monitorErr := stopMonitor(); monitorErr != nil {
				returnErr = errors.Join(returnErr, monitorErr)
				result = nil
			}
		}
		var cleanupErr error
		if v.attempt != nil {
			if v.attempt.JoinAndClose == nil {
				cleanupErr = errors.New("background: attempt returned resources without cleanup ownership")
			} else {
				cleanupErr = joinAttempt(context.WithoutCancel(ctx), v.attempt)
			}
		}
		state.mu.Lock()
		state.joined = cleanupErr == nil
		state.ready = true
		state.failed = returnErr != nil || cleanupErr != nil
		state.cleanupErr = cleanupErr
		state.mu.Unlock()
		if cleanupErr != nil {
			_, persistErr := s.store.DB().ExecContext(context.WithoutCancel(ctx), "UPDATE harness_background_bindings SET blocked_reason=? WHERE task_id=?", cleanupErr.Error(), b.TaskID)
			s.mu.Lock()
			s.cleanupErr = errors.Join(s.cleanupErr, cleanupErr, persistErr)
			s.mu.Unlock()
			returnErr = errors.Join(returnErr, harness.ErrBackgroundUncertain, cleanupErr, persistErr)
			result = nil
		}
	}()
	if err = s.config.Budgets.BeforeAttempt(ctx, state.scope); err != nil {
		return nil, err
	}
	if monitor, ok := s.config.Budgets.(BudgetMonitor); ok {
		ctx, stopMonitor = startBudgetMonitor(ctx, monitor, state.scope, s.config.HeartbeatInterval)
	}
	if s.config.Attempts != nil {
		v.attempt, err = s.config.Attempts.Open(ctx, state.scope)
		if err != nil {
			return nil, err
		}
		if v.attempt == nil || v.attempt.JoinAndClose == nil {
			return nil, errors.New("background: attempt factory must provide cleanup ownership")
		}
		if b.ExecutorKey == ds.ExecutorKey && (v.attempt.Agent == nil || v.attempt.Agent.Name(ctx) != b.AgentVersion) {
			return nil, errors.New("background: rebuilt agent name does not match durable registration")
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return e.inner.Execute(ctx, task, runtime)
}

func joinAttempt(ctx context.Context, attempt *Attempt) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("background cleanup panic: %v", recovered)
		}
	}()
	return attempt.JoinAndClose(ctx)
}

type resumeContextKey struct{}
type resumeContext struct {
	actor   harness.TaskActor
	binding Binding
	grant   ResumeGrant
}

func (s *Service) ResolveApproval(ctx context.Context, actor harness.TaskActor, id string, request harness.TaskApproval) (harness.BackgroundTask, error) {
	b, _, err := s.authorizedBinding(ctx, actor, id)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	if s.config.Approvals == nil {
		return harness.BackgroundTask{}, harness.ErrBackgroundUnavailable
	}
	grant, err := s.config.Approvals.PrepareResume(ctx, actor, b, request)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	if !json.Valid(grant.Data) || request.TaskVersion < 1 {
		return harness.BackgroundTask{}, harness.ErrInvalidInput
	}
	ctx = context.WithValue(ctx, resumeContextKey{}, &resumeContext{actor: actor, binding: b, grant: grant})
	if _, err = s.manager.Resume(ctx, &bt.ResumeRequest{TaskID: id, ExpectedVersion: request.TaskVersion, Data: grant.Data}); err != nil {
		return harness.BackgroundTask{}, err
	}
	s.wake()
	return s.Get(ctx, actor, id)
}

// ReleaseSuspension is explicit; startup never silently resumes paused work.
func (s *Service) ReleaseSuspension(ctx context.Context, actor harness.TaskActor, id string) (harness.BackgroundTask, error) {
	b, blocked, err := s.authorizedBinding(ctx, actor, id)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	if blocked != "" {
		return harness.BackgroundTask{}, harness.ErrBackgroundUncertain
	}
	if s.config.Attempts != nil {
		if err = s.config.Attempts.Validate(ctx, b); err != nil {
			return harness.BackgroundTask{}, err
		}
	}
	if _, err = s.manager.ReleaseSuspension(ctx, id); err != nil {
		return harness.BackgroundTask{}, err
	}
	s.wake()
	return s.Get(ctx, actor, id)
}
