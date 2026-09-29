package background

import (
	"context"
	"errors"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// StartWorkers starts bounded dispatch and outbox intake. Context cancellation
// stops dispatch only; DrainAndClose owns cancellation/drain of running tasks.
func (s *Service) StartWorkers(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return harness.ErrBackgroundClosed
	}
	if s.started {
		return errors.New("background: workers already started")
	}
	s.started = true
	dispatch, stop := context.WithCancel(ctx)
	s.dispatchCancel = stop
	for i := 0; i < s.config.MaxWorkers; i++ {
		s.workers.Add(1)
		go s.worker(context.WithoutCancel(ctx))
	}
	go func() { s.workers.Wait(); close(s.workersDone) }()
	go s.scan(dispatch)
	return nil
}

func (s *Service) scan(ctx context.Context) {
	defer close(s.scanDone)
	defer close(s.jobs)
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.dispatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.report(err)
		}
		if err := s.DeliverNotifications(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wakeup:
		}
	}
}

func (s *Service) dispatch(ctx context.Context) error {
	keys := s.registry.Keys()
	if len(keys) == 0 {
		return nil
	}
	cursor := ""
	for {
		page, err := s.manager.ListPending(ctx, &bt.ListPendingRequest{ExecutorKeys: keys, Cursor: cursor, Limit: 100})
		if err != nil {
			return err
		}
		for _, task := range page.Tasks {
			_, blocked, bindingErr := loadBinding(ctx, s.store.DB(), task.Spec.ID)
			if errors.Is(bindingErr, harness.ErrNotFound) {
				continue
			} else if bindingErr != nil {
				return bindingErr
			} else if blocked != "" {
				continue
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return nil
			}
			if s.inflight[task.Spec.ID] || s.backoff[task.Spec.ID].After(time.Now()) {
				s.mu.Unlock()
				continue
			}
			s.inflight[task.Spec.ID] = true
			s.mu.Unlock()
			select {
			case s.jobs <- task.Spec.ID:
			case <-ctx.Done():
				s.mu.Lock()
				delete(s.inflight, task.Spec.ID)
				s.mu.Unlock()
				return ctx.Err()
			default:
				// A full queue must not starve outbox intake while workers
				// are blocked in long executions. The next scan retries it.
				s.mu.Lock()
				delete(s.inflight, task.Spec.ID)
				s.mu.Unlock()
				return nil
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

func (s *Service) worker(ctx context.Context) {
	defer s.workers.Done()
	for id := range s.jobs {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		var err error
		if !closed {
			err = s.manager.Execute(ctx, id)
		}
		s.mu.Lock()
		delete(s.inflight, id)
		if err != nil {
			s.backoff[id] = time.Now().Add(max(s.config.PollInterval, 250*time.Millisecond))
		} else {
			delete(s.backoff, id)
		}
		for taskID, until := range s.backoff {
			if until.Before(time.Now()) {
				delete(s.backoff, taskID)
			}
		}
		// Joined execution can still have an uncommitted terminal transaction.
		// Keep its diagnostic, checkpoint and ledger state until a successful
		// transition; lease recovery must not silently skip these projections.
		if err == nil {
			for key, state := range s.attempts {
				if state.scope.Binding.TaskID == id && state.isJoined() {
					delete(s.attempts, key)
				}
			}
		}
		s.mu.Unlock()
		if err != nil && !errors.Is(err, harness.ErrChildSessionBusy) && !errors.Is(err, bt.ErrVersionConflict) && !errors.Is(err, bt.ErrIllegalTransition) {
			s.report(err)
		}
		s.wake()
	}
}

// DrainAndClose returns nil only after the dispatcher, native executions and
// their injected cleanup have returned. It never closes the shared database.
// A deadline error leaves the service closed to submissions; callers may wait
// again, and must retain database/resources while executions are still alive.
func (s *Service) DrainAndClose(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		return bt.ErrCloseDeadlineRequired
	}
	s.mu.Lock()
	s.closed = true
	started := s.started
	stop := s.dispatchCancel
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	if started {
		select {
		case <-s.scanDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := s.manager.Close(ctx, bt.WithDrainReason("harness shutdown"))
	if started {
		select {
		case <-s.workersDone:
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	s.mu.Lock()
	cleanupErr := s.cleanupErr
	s.mu.Unlock()
	if cleanupErr != nil {
		err = errors.Join(err, harness.ErrBackgroundUncertain, cleanupErr)
	}
	return err
}
