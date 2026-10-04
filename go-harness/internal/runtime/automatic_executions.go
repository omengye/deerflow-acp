package runtime

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var errAutomaticDeferred = errors.New("automatic execution is deferred or stopped")

type automaticExecutionKey struct{}

// Waiting execution manifests are the durable queue. This table only records
// retry spending for the exact checkpoint; reconnect/restart cannot reset it.
func executionAutomaticState(ctx context.Context, tx *sql.Tx, row executionRow) (*harness.ExecutionAutomatic, error) {
	if row.State.Status != harness.ExecutionWaitingInput || row.Manifest == nil {
		return nil, nil
	}
	var stopped bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_automatic_stops WHERE root_id=?)`, row.RootBudgetID).Scan(&stopped); err != nil {
		return nil, err
	}
	if stopped {
		return &harness.ExecutionAutomatic{Status: "stopped", Detail: "Automatic work for this root was explicitly stopped."}, nil
	}
	var attempts int
	var next int64
	err := tx.QueryRowContext(ctx, `SELECT attempts,next_at FROM harness_execution_wakeups WHERE run_id=? AND checkpoint_sha=?`, row.State.RunID, row.Manifest.CheckpointSHA).Scan(&attempts, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state := &harness.ExecutionAutomatic{Attempts: attempts, Status: "retrying"}
	if attempts >= 3 {
		state.Status, state.Detail = "blocked", "Automatic recovery limit reached. Inspect this execution before explicitly resuming."
	} else {
		when := time.Unix(0, next).UTC()
		state.RetryAfter = &when
	}
	return state, nil
}

func claimAutomaticExecutionTx(ctx context.Context, tx *sql.Tx, row executionRow) error {
	auto, _ := ctx.Value(automaticExecutionKey{}).(bool)
	if !auto {
		return nil
	}
	state, err := executionAutomaticState(ctx, tx, row)
	if err != nil {
		return err
	}
	now := time.Now()
	if state != nil && (state.Status == "stopped" || state.Status == "blocked" || state.RetryAfter != nil && now.Before(*state.RetryAfter)) {
		return errAutomaticDeferred
	}
	attempts := 1
	if state != nil {
		attempts += state.Attempts
	}
	delay := time.Second
	if attempts > 1 {
		delay = 5 * time.Second
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_execution_wakeups(run_id,checkpoint_sha,attempts,next_at) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET checkpoint_sha=excluded.checkpoint_sha,attempts=excluded.attempts,next_at=excluded.next_at`, row.State.RunID, row.Manifest.CheckpointSHA, attempts, now.Add(delay).UnixNano())
	return err
}

func stopAutomaticRootTx(ctx context.Context, tx *sql.Tx, row executionRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO harness_automatic_stops(root_id,session_id,reason,created_at) VALUES(?,?,'explicit cancellation',?) ON CONFLICT(root_id) DO NOTHING`, row.RootBudgetID, row.State.SessionID, timestamp())
	if err != nil {
		return err
	}
	var native bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_background_continuation_sources')`).Scan(&native); err != nil || !native {
		return err
	}
	// Stop already admitted automatic native work as well. Native cancellation
	// scanners deliver these durable fences and join effects before settlement.
	_, err = tx.ExecContext(ctx, `WITH RECURSIVE stopped(id) AS (
 SELECT task_id FROM harness_background_continuation_sources WHERE root_id=? AND json_extract(CAST(payload AS TEXT),'$.automatic')=1
 UNION SELECT t.task_id FROM harness_background_tree t JOIN stopped s ON t.parent_task_id=s.id
 UNION SELECT l.task_id FROM harness_background_continuation_links l JOIN stopped s ON l.previous_task_id=s.id
 ) INSERT INTO harness_background_stops(task_id,reason,created_at) SELECT id,'root automatic continuation stopped',? FROM stopped WHERE true ON CONFLICT(task_id) DO NOTHING`, row.RootBudgetID, timestamp())
	return err
}

// Cancel stops current execution work, including an idle durable wait. It also
// disables automatic work for the same root. Unlike disconnect, it is a user
// stop. Cancellation during I/O is committed by the joined attempt lifecycle.
func (s *Service) Cancel(ctx context.Context, owner, sessionID string) error {
	ctx, release, err := s.Coordinator.CancelOrBegin(ctx, sessionID, owner)
	if err != nil || release == nil {
		return err
	}
	defer release()
	return withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var runID string
		err := tx.QueryRowContext(ctx, `SELECT run_id FROM harness_executions WHERE session_id=? ORDER BY created_at DESC,run_id DESC LIMIT 1`, sessionID).Scan(&runID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		row, err := readExecution(ctx, tx, sessionID, runID)
		if err != nil {
			return err
		}
		if row.State.Status == harness.ExecutionWaitingInput || row.State.Status == harness.ExecutionNeedsReconciliation {
			_, err = s.Store.CancelExecutionTx(ctx, tx, sessionID, owner, harness.CancelExecutionRequest{RunID: runID, ExpectedVersion: row.State.Version})
			return err
		}
		return stopAutomaticRootTx(ctx, tx, row)
	})
}

type sessionUpdatesSubscription struct {
	owner, session string
	cancel         context.CancelFunc
	done           chan struct{}
}

func (s *Service) stopSessionUpdates(owner, sessionID string) []*sessionUpdatesSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	var subscriptions []*sessionUpdatesSubscription
	for _, sub := range s.sessionUpdates {
		if sub.owner == owner && (sessionID == "" || sub.session == sessionID) {
			sub.cancel()
			subscriptions = append(subscriptions, sub)
		}
	}
	return subscriptions
}

func joinSessionUpdates(ctx context.Context, subscriptions []*sessionUpdatesSubscription) error {
	for _, sub := range subscriptions {
		select {
		case <-sub.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ServeSessionUpdates binds automatic output and fresh permission requests to
// a live owner. It does not reuse handlers supplied to an earlier prompt.
// The waiting manifest survives stopping this subscription or disconnecting.
func (s *Service) ServeSessionUpdates(parent context.Context, owner, sessionID string, handlers harness.SessionUpdates) error {
	if s.executionEngine() == nil {
		return executionInvalid("automatic updates require durable execution")
	}
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(WithTransportCancellation(parent))
	defer cancel()
	started := false
	if handlers.Started != nil {
		ctx = context.WithValue(ctx, automaticStartedKey{}, func(ctx context.Context, state harness.ExecutionState) error {
			started = true
			return handlers.Started(ctx, state)
		})
	}
	sub := &sessionUpdatesSubscription{owner: owner, session: sessionID, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	// Register while holding the same mutex used by cleanup. Either cleanup
	// sees this subscription, or the coordinator's closing fence rejects it.
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.sessionUpdates == nil {
		s.sessionUpdates = map[string]*sessionUpdatesSubscription{}
	}
	if s.sessionUpdates[sessionID] != nil {
		s.mu.Unlock()
		return harness.ErrBusy
	}
	s.sessionUpdates[sessionID] = sub
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.sessionUpdates, sessionID); close(sub.done); s.mu.Unlock() }()
	// Recheck after registration: a disconnect between the first authorization
	// and registration must not leave an orphan pump or callback after Close.
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return err
	}
	if handlers.Permission == nil {
		handlers.Permission = func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.PermissionCancelled, nil
		}
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
			if errors.Is(err, harness.ErrNotAttached) {
				return nil
			}
			return err
		}
		state, err := s.Execution(ctx, owner, sessionID, "")
		if errors.Is(err, harness.ErrNotFound) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var result harness.RunResult
		var runErr error
		newRun := false
		started = false
		if state.Status == harness.ExecutionWaitingInput {
			if !state.Resumable || len(state.WaitingInputs) > 0 || len(state.WaitingTasks) == 0 {
				continue
			}
			if a := state.Automatic; a != nil && (a.Status == "blocked" || a.Status == "stopped" || a.RetryAfter != nil && time.Now().Before(*a.RetryAfter)) {
				continue
			}
			result, runErr = s.resumeAutomaticExecution(ctx, owner, sessionID, state, handlers)
		} else if state.Status == harness.ExecutionCompleted || state.Status == harness.ExecutionCancelled || state.Status == harness.ExecutionFailed {
			id, err := s.nextAutomaticNotification(ctx, owner, sessionID)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if id == "" {
				continue
			}
			newRun = true
			result, runErr = s.processAutomaticNotification(ctx, owner, sessionID, id, handlers)
		} else {
			continue
		}
		if !started && (errors.Is(runErr, harness.ErrBusy) || errors.Is(runErr, harness.ErrExecutionConflict) || errors.Is(runErr, harness.ErrExecutionWaitingInput) || errors.Is(runErr, harness.ErrResourcesWaiting) || errors.Is(runErr, harness.ErrResourcesIncompatible) || errors.Is(runErr, errAutomaticDeferred)) {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
			return nil
		}
		if handlers.Result != nil {
			if err := handlers.Result(ctx, result, runErr); err != nil {
				return err
			}
		} else if runErr != nil {
			return runErr
		}
		// Failures before a claim have no durable retry charge (for example a
		// spent budget). Stop the subscription instead of reporting them forever.
		if runErr != nil && !newRun && (result.Execution == nil || result.Execution.Attempt == state.Attempt) {
			return runErr
		}
	}
}

func (s *Service) resumeAutomaticExecution(ctx context.Context, owner, sessionID string, state harness.ExecutionState, handlers harness.SessionUpdates) (harness.RunResult, error) {
	ctx, releaseSlot, err := s.runQueue.tryAutomatic(ctx)
	if err != nil {
		return harness.RunResult{}, err
	}
	defer releaseSlot()
	ctx = context.WithValue(ctx, automaticExecutionKey{}, true)
	return s.ResumeExecution(ctx, owner, sessionID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, handlers.Events, handlers.Permission)
}
