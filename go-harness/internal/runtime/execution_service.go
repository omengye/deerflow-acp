package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type transportCancellationKey struct{}

// WithTransportCancellation marks a connection-owned request. If its parent
// dies before explicit session/cancel, waiting work survives for another owner.
// SDK caller cancellation remains explicit by default.
func WithTransportCancellation(ctx context.Context) context.Context {
	return context.WithValue(ctx, transportCancellationKey{}, true)
}

func disconnectedExecution(ctx context.Context) bool {
	if errors.Is(context.Cause(ctx), session.ErrExplicitCancel) {
		return false
	}
	if errors.Is(context.Cause(ctx), session.ErrDisconnected) {
		return true
	}
	marked, _ := ctx.Value(transportCancellationKey{}).(bool)
	return marked && ctx.Err() != nil
}

type executionEngine interface {
	DurableExecutions() bool
	Resume(context.Context, harness.RunRequest, string, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error)
}

func (s *Service) executionEngine() executionEngine {
	engine, ok := s.Engine.(executionEngine)
	if !ok || !engine.DurableExecutions() || s.Store.BudgetLedger == nil {
		return nil
	}
	return engine
}

func (s *Service) DurableExecutionsEnabled() bool { return s.executionEngine() != nil }

type executionLeaseKey struct{}
type executionAttemptKey struct{}

func withExecutionLease(ctx context.Context, lease ExecutionLease) context.Context {
	return context.WithValue(ctx, executionLeaseKey{}, lease)
}

func withExecutionTransaction(ctx context.Context, store *Store, fn func(*sql.Tx) error) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type executionAttempt struct {
	service    *Service
	lease      ExecutionLease
	grants     map[string]PermissionGrant
	requests   map[string]harness.PermissionRequest
	mu         sync.Mutex
	checkpoint *interaction.StagedExecutionCheckpoint
	eventErr   error
}

func (a *executionAttempt) PreparePermission(ctx context.Context, p harness.PermissionRequest) (interaction.PermissionIntent, error) {
	var intent harness.ExecutionInteraction
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := withExecutionTransaction(persist, a.service.Store, func(tx *sql.Tx) error {
		var err error
		intent, err = a.service.Store.RecordPermissionIntentTx(persist, tx, a.lease, p)
		return err
	})
	return interaction.PermissionIntent{ID: intent.ID, Version: intent.Version}, err
}

func (a *executionAttempt) ResolvePermission(ctx context.Context, p harness.PermissionRequest, intent interaction.PermissionIntent, grantID string) (harness.PermissionDecision, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := a.service.Coordinator.Authorize(p.SessionID, a.lease.OwnerID); err != nil {
		return "", err
	}
	grant, ok := a.grants[p.ToolCallID]
	if !ok || grant.ID != grantID || grant.IntentID != intent.ID || grant.IntentVersion != intent.Version {
		return "", harness.ErrExecutionConflict
	}
	var decision harness.PermissionDecision
	err := withExecutionTransaction(ctx, a.service.Store, func(tx *sql.Tx) error {
		var err error
		decision, err = a.service.Store.ValidatePermissionGrantTx(ctx, tx, a.lease, grant, p)
		return err
	})
	return decision, err
}

func (a *executionAttempt) stage(_ context.Context, cp interaction.StagedExecutionCheckpoint) error {
	if cp.ID != "harness/turn/v1/"+a.lease.Scope.MemberID || (!cp.Remove && (len(cp.Data) == 0 || len(cp.Data) > 64*1024*1024)) {
		return executionInvalid("invalid staged native checkpoint")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.checkpoint != nil {
		return harness.ErrExecutionConflict
	}
	cp.Data = append([]byte(nil), cp.Data...)
	cp.Interrupts = append([]interaction.ExecutionInterruptBinding(nil), cp.Interrupts...)
	a.checkpoint = &cp
	return nil
}

func (a *executionAttempt) publish(emit harness.EventHandler) harness.EventHandler {
	return func(ctx context.Context, e harness.RunEvent) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.eventErr != nil {
			return a.eventErr
		}
		e.SessionID, e.RunID = a.lease.Scope.SessionID, a.lease.Scope.MemberID
		if e.Kind == "tool_execute" {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := a.service.Coordinator.Authorize(e.SessionID, a.lease.OwnerID); err != nil {
				return err
			}
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		persist = context.WithValue(persist, executionAttemptKey{}, a)
		saved, err := a.service.Store.Append(persist, e, a.service.Assets)
		if err != nil {
			return &runPersistenceError{err: err}
		}
		// Native resume revisits a pending call. Its original card and receipt
		// stay unchanged; only the eventual execute/deny event is new.
		if saved.Sequence == 0 {
			return nil
		}
		if emit != nil {
			if err := emit(ctx, saved); err != nil {
				// Event handlers are execution gates, including before an effect.
				// Persistence succeeding does not permit ignoring a failed gate.
				a.eventErr = err
				return err
			}
		}
		return nil
	}
}

func (s *Service) runDurableNew(ctx context.Context, owner string, req harness.RunRequest, prepared *assets.Prepared, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	lease := ExecutionLease{Scope: foregroundBudgetScope(req), OwnerID: owner, InputID: req.InputID}
	if err := s.Store.BeginRun(withExecutionLease(ctx, lease), req, prepared); err != nil {
		if prepared != nil {
			err = errors.Join(err, prepared.Finish(false))
		}
		return harness.RunResult{}, err
	}
	if prepared != nil {
		if err := prepared.Finish(true); err != nil {
			return s.finishExecutionAttempt(ctx, lease, harness.RunResult{}, err, nil)
		}
	}
	return s.driveExecution(ctx, req, lease, nil, emit, approve)
}

func (s *Service) driveExecution(ctx context.Context, req harness.RunRequest, lease ExecutionLease, bindings []ExecutionResumeBinding, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	for {
		result, err := s.executeAttempt(ctx, req, lease, bindings, emit, approve)
		if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput || result.StopReason != "waiting_input" {
			if result.StopReason == "waiting_input" {
				result.StopReason = "end_turn"
			}
			return result, err
		}
		owner := lease.OwnerID
		lease, bindings, err = s.prepareExecutionResume(ctx, owner, req.Session.ID, harness.ResumeExecutionRequest{RunID: req.RunID, ExpectedVersion: result.Execution.Version}, approve)
		if err != nil {
			return s.waitingExecutionResult(ctx, owner, req.Session.ID, req.RunID, err)
		}
	}
}

func (s *Service) executeAttempt(ctx context.Context, req harness.RunRequest, lease ExecutionLease, bindings []ExecutionResumeBinding, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	var inputSource *interaction.ExecutionInputSource
	err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		row, err := checkExecutionLease(ctx, tx, lease)
		if err != nil {
			return err
		}
		if err = validateExecutionConfig(ctx, tx, row); err != nil {
			return err
		}
		inputSource, err = s.executionInputSourceTx(ctx, tx, row)
		return err
	})
	if err != nil {
		return s.finishExecutionAttempt(ctx, lease, harness.RunResult{}, err, nil)
	}
	a := &executionAttempt{service: s, lease: lease, grants: make(map[string]PermissionGrant), requests: make(map[string]harness.PermissionRequest)}
	targets := make(map[string]interaction.PermissionResume, len(bindings))
	if len(bindings) > 0 {
		err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
			requests, err := s.Store.PendingExecutionPermissionsTx(ctx, tx, lease)
			if err != nil {
				return err
			}
			byIntent := make(map[string]harness.PermissionRequest, len(requests))
			for _, p := range requests {
				byIntent[p.ID] = p
			}
			for _, b := range bindings {
				p, ok := byIntent[b.Interrupt.IntentID]
				if !ok {
					return harness.ErrExecutionConflict
				}
				a.grants[p.ToolCallID], a.requests[p.ToolCallID] = b.Grant, p
				targets[b.Interrupt.NativeInterruptID] = interaction.PermissionResume{IntentID: b.Grant.IntentID, IntentVersion: b.Grant.IntentVersion, GrantID: b.Grant.ID}
			}
			return nil
		})
		if err != nil {
			return s.finishExecutionAttempt(ctx, lease, harness.RunResult{}, err, nil)
		}
	}
	runCtx := context.WithValue(ctx, taskActorKey{}, harness.TaskActor{OwnerID: lease.OwnerID, SessionID: req.Session.ID})
	runCtx, stopHeartbeat := keepBudgetAlive(budget.WithScope(runCtx, lease.Scope), s.Store.BudgetLedger, lease.Scope)
	runCtx = interaction.WithExecutionHooks(runCtx, interaction.ExecutionHooks{Broker: a, Targets: targets, StageCheckpoint: a.stage, InputSource: inputSource})
	var result harness.RunResult
	var runErr error
	// Native tools still use the checkpoint broker. This live handler serves
	// nested permission requests from an external ACP process while the owning
	// connection exists. A disconnect cancels the process and leaves its outer
	// started receipt uncertain; it cannot resume that remote prompt in place.
	livePermission := func(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		return s.executionPermission(ctx, lease.OwnerID, req.Session, p, approve)
	}
	if len(bindings) == 0 {
		result, runErr = s.Engine.Run(runCtx, req, a.publish(emit), livePermission)
	} else {
		result, runErr = s.executionEngine().Resume(runCtx, req, "harness/turn/v1/"+req.RunID, a.publish(emit), livePermission)
	}
	if s.Assets != nil {
		runErr = errors.Join(runErr, s.Assets.AbortRun(req.RunID))
	}
	runErr = errors.Join(runErr, applyBudgetHeartbeat(&result, stopHeartbeat()))
	// Retain callback failures even if an engine incorrectly discards one. The
	// engine has joined here, so checkpoint/error fields are no longer changing.
	runErr = errors.Join(runErr, a.eventErr)
	return s.finishExecutionAttempt(ctx, lease, result, runErr, a.checkpoint)
}

func (s *Service) finishExecutionAttempt(ctx context.Context, lease ExecutionLease, result harness.RunResult, runErr error, cp *interaction.StagedExecutionCheckpoint) (harness.RunResult, error) {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	explicitCancel := ctx.Err() != nil && !disconnectedExecution(ctx)
	var state harness.ExecutionState
	err := withExecutionTransaction(persist, s.Store, func(tx *sql.Tx) error {
		var err error
		outcome := budget.OutcomeCompleted
		if runErr == nil && result.StopReason == "waiting_input" && cp != nil && !cp.Remove && !explicitCancel {
			checkpoint := ExecutionCheckpoint{ID: cp.ID, Data: cp.Data}
			for _, b := range cp.Interrupts {
				checkpoint.Interrupts = append(checkpoint.Interrupts, ExecutionInterruptBinding{IntentID: b.IntentID, IntentVersion: b.IntentVersion, NativeInterruptID: b.NativeInterruptID})
			}
			state, err = s.Store.SuspendExecutionTx(persist, tx, lease, checkpoint)
			outcome = budget.OutcomeWaitingInput
		} else {
			kind := harness.ExecutionStopCompleted
			if explicitCancel || result.StopReason == "cancelled" && !disconnectedExecution(ctx) {
				kind, outcome = harness.ExecutionStopCancelled, budget.OutcomeCancelled
				result.StopReason = "cancelled"
				if cancellationOnly(runErr) {
					runErr = nil
				}
			} else if disconnectedExecution(ctx) {
				kind, outcome = harness.ExecutionStopDisconnected, budget.OutcomeInterrupted
			} else if runErr != nil {
				kind, outcome = harness.ExecutionStopFailed, budget.OutcomeFailed
			} else if result.StopReason == "waiting_input" {
				runErr = errors.New("native interruption did not stage a checkpoint")
				kind, outcome = harness.ExecutionStopFailed, budget.OutcomeFailed
			}
			detail := ""
			if runErr != nil {
				detail = runErr.Error()
			}
			state, err = s.Store.EndExecutionAttemptTx(persist, tx, lease, kind, detail)
			if state.Status == harness.ExecutionWaitingInput {
				outcome = budget.OutcomeWaitingInput
			}
		}
		if err != nil {
			return err
		}
		if err = s.Store.BudgetLedger.EndAttemptTx(persist, tx, lease.Scope, outcome); err != nil {
			return err
		}
		if s.Memory != nil {
			publish := state.Status == harness.ExecutionCompleted && runErr == nil && result.StopReason == "end_turn"
			if _, err = s.Memory.SettleStagingTx(persist, tx, lease.Scope.MemberID, lease.Scope.AttemptID, publish); err != nil {
				return err
			}
		}
		if state.Status != harness.ExecutionWaitingInput {
			if result.StopReason == "" {
				result.StopReason = "end_turn"
			}
			_, err = tx.ExecContext(persist, `UPDATE harness_runs SET stop_reason=? WHERE id=?`, result.StopReason, lease.Scope.MemberID)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(persist, `UPDATE harness_sessions SET updated_at=? WHERE id=?`, timestamp(), lease.Scope.SessionID)
		return err
	})
	if err != nil {
		return result, errors.Join(runErr, &runPersistenceError{err: err})
	}
	state, err = s.Store.Execution(persist, lease.Scope.SessionID, lease.Scope.MemberID)
	if err != nil {
		return result, errors.Join(runErr, &runPersistenceError{err: err})
	}
	result.Execution = &state
	return result, runErr
}

type executionAnswer struct {
	intent   harness.PermissionRequest
	version  int64
	decision harness.PermissionDecision
}

// Permission I/O occurs while the previous budget attempt is ended. Only after
// all answers are available is a new attempt claimed and charged for execution.
func (s *Service) prepareExecutionResume(ctx context.Context, owner, sessionID string, request harness.ResumeExecutionRequest, approve harness.PermissionHandler) (lease ExecutionLease, bindings []ExecutionResumeBinding, err error) {
	lease.OwnerID = owner
	var config harness.Session
	var answers []executionAnswer
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, sessionID, request.RunID)
		if err != nil {
			return err
		}
		if row.State.Status != harness.ExecutionWaitingInput || row.State.Version != request.ExpectedVersion {
			return harness.ErrExecutionConflict
		}
		if err = validateExecutionManifest(ctx, tx, row); err != nil {
			return err
		}
		if _, err = s.executionInputSourceTx(ctx, tx, row); err != nil {
			return err
		}
		config = row.Config
		for _, b := range row.Manifest.Interrupts {
			p, version, state, err := readExecutionIntent(ctx, tx, request.RunID, b.IntentID)
			if err != nil {
				return err
			}
			if state != "pending" || version != b.IntentVersion {
				return harness.ErrExecutionConflict
			}
			answers = append(answers, executionAnswer{intent: p, version: version})
		}
		return nil
	})
	if err != nil {
		return
	}
	for i := range answers {
		if err = ctx.Err(); err != nil {
			return
		}
		if err = s.Coordinator.Authorize(sessionID, owner); err != nil {
			return
		}
		answers[i].decision, err = s.executionPermission(ctx, owner, config, answers[i].intent, approve)
		if err != nil {
			return
		}
		if answers[i].decision == harness.PermissionCancelled {
			err = harness.ErrExecutionWaitingInput
			return
		}
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if err = s.Coordinator.Authorize(sessionID, owner); err != nil {
		return
	}
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var err error
		row, err := readExecution(ctx, tx, sessionID, request.RunID)
		if err != nil {
			return err
		}
		if _, err = s.executionInputSourceTx(ctx, tx, row); err != nil {
			return err
		}
		lease, err = s.Store.ClaimExecutionResumeTx(ctx, tx, sessionID, owner, request, NewID())
		if err != nil {
			return err
		}
		if err = s.Store.BudgetLedger.BeginAttemptTx(ctx, tx, lease.Scope); err != nil {
			return err
		}
		for _, answer := range answers {
			if _, err = s.Store.RecordPermissionGrantTx(ctx, tx, lease, answer.intent.ID, answer.version, answer.decision); err != nil {
				return err
			}
		}
		bindings, err = s.Store.ExecutionResumeBindingsTx(ctx, tx, lease)
		return err
	})
	return
}

func (s *Service) executionPermission(ctx context.Context, owner string, config harness.Session, p harness.PermissionRequest, approve harness.PermissionHandler) (harness.PermissionDecision, error) {
	data, err := json.Marshal(struct {
		Tool          string
		Args          json.RawMessage
		ConfigVersion int64
	}{p.ToolName, p.Arguments, p.ConfigVersion})
	if err != nil {
		return "", err
	}
	key := owner + "/" + config.ID + "/" + fmt.Sprintf("%x", sha256.Sum256(data))
	decision, cached := configuredPermission(config, p)
	if !cached {
		s.mu.Lock()
		decision, cached = s.decisions[key]
		s.mu.Unlock()
	}
	if !cached {
		decision = harness.RejectOnce
		if approve != nil {
			decision, err = approve(ctx, p)
		}
	}
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return harness.PermissionCancelled, err
	}
	switch decision {
	case harness.AllowOnce, harness.AllowAlways, harness.RejectOnce, harness.RejectAlways, harness.PermissionCancelled:
	default:
		return "", executionInvalid("invalid execution permission decision")
	}
	if decision == harness.AllowAlways || decision == harness.RejectAlways {
		s.mu.Lock()
		s.decisions[key] = decision
		s.mu.Unlock()
	}
	return decision, nil
}

func (s *Service) waitingExecutionResult(ctx context.Context, owner, sessionID, runID string, cause error) (harness.RunResult, error) {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	state, err := s.Store.Execution(persist, sessionID, runID)
	if err != nil {
		return harness.RunResult{}, errors.Join(cause, err)
	}
	result := harness.RunResult{StopReason: "end_turn", Execution: &state}
	if ctx.Err() != nil && !disconnectedExecution(ctx) && state.Status == harness.ExecutionWaitingInput {
		err = withExecutionTransaction(persist, s.Store, func(tx *sql.Tx) error {
			var err error
			state, err = s.Store.CancelExecutionTx(persist, tx, sessionID, owner, harness.CancelExecutionRequest{RunID: runID, ExpectedVersion: state.Version})
			return err
		})
		result.StopReason, result.Execution = "cancelled", &state
		if cancellationOnly(cause) {
			cause = nil
		}
		return result, errors.Join(cause, err)
	}
	if errors.Is(cause, harness.ErrExecutionWaitingInput) || disconnectedExecution(ctx) && cancellationOnly(cause) {
		cause = nil
	}
	return result, cause
}

func (s *Service) Execution(ctx context.Context, owner, sessionID, runID string) (harness.ExecutionState, error) {
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return harness.ExecutionState{}, err
	}
	state, err := s.Store.Execution(ctx, sessionID, runID)
	if err != nil || state.Status != harness.ExecutionWaitingInput || !state.Resumable {
		return state, err
	}
	// The store validates immutable source bytes; the host additionally pins the
	// current execution policy. Report drift before displaying a resume action.
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, sessionID, state.RunID)
		if err != nil {
			return err
		}
		_, err = s.executionInputSourceTx(ctx, tx, row)
		return err
	})
	if errors.Is(err, harness.ErrExecutionUnresumable) || errors.Is(err, harness.ErrReconciliationRequired) || errors.Is(err, harness.ErrExecutionConflict) {
		state.Resumable = false
		state.BlockedReason = err.Error()
		return state, nil
	}
	return state, err
}

func (s *Service) ResumeExecution(ctx context.Context, owner, sessionID string, request harness.ResumeExecutionRequest, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	if s.executionEngine() == nil {
		return harness.RunResult{}, executionInvalid("engine does not support durable execution")
	}
	if r, ok := ctx.Value(reservationKey{}).(*reservation); ok && r.sessionID == sessionID && r.owner == owner {
		if !r.used.CompareAndSwap(false, true) {
			return harness.RunResult{}, harness.ErrBusy
		}
	} else {
		var release func()
		var err error
		ctx, release, err = s.Admit(ctx, owner, sessionID)
		if err != nil {
			return harness.RunResult{}, err
		}
		defer release()
		ctx.Value(reservationKey{}).(*reservation).used.Store(true)
	}
	lease, bindings, err := s.prepareExecutionResume(ctx, owner, sessionID, request, approve)
	if err != nil {
		return s.waitingExecutionResult(ctx, owner, sessionID, request.RunID, err)
	}
	var req harness.RunRequest
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var err error
		req, err = s.Store.ExecutionRequestTx(ctx, tx, lease)
		return err
	})
	if err != nil {
		return s.finishExecutionAttempt(ctx, lease, harness.RunResult{}, err, nil)
	}
	return s.driveExecution(ctx, req, lease, bindings, emit, approve)
}

func (s *Service) CancelExecution(ctx context.Context, owner, sessionID string, request harness.CancelExecutionRequest) (harness.ExecutionState, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return harness.ExecutionState{}, err
	}
	defer release()
	var state harness.ExecutionState
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var err error
		state, err = s.Store.CancelExecutionTx(ctx, tx, sessionID, owner, request)
		return err
	})
	return state, err
}
