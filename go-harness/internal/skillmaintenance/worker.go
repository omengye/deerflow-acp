package skillmaintenance

import (
	"context"
	"errors"
	"time"
)

// Handler receives a host-owned claim, with the broker and fixed job deadline
// in its context. It must check producer input identity and source authority
// before model I/O, join cancellation, and never publish directly.
type Handler func(context.Context, Job) (code string, retry bool)

// RunOne claims at most one job and joins its heartbeat and handler. A host may
// drive a bounded worker loop; this package does not start a hidden scheduler.
func (s *Store) RunOne(ctx context.Context, handler Handler) (bool, error) {
	if handler == nil {
		return false, ErrInvalid
	}
	claim, ok, err := s.Claim(ctx)
	if err != nil || !ok {
		return false, err
	}
	work, cancel := context.WithDeadline(ctx, claim.Job.Deadline)
	defer cancel()
	work = WithBroker(work, s.Broker(claim))
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(leaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				done <- nil
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(work, 2*time.Second)
				err := s.Heartbeat(check, claim)
				stop()
				if err != nil {
					cancel()
					done <- err
					return
				}
			}
		}
	}()
	code, retry := handler(work, claim.Job)
	workErr := work.Err()
	cancel()
	heartbeatErr := <-done
	if heartbeatErr != nil {
		code, retry = "storage_error", true
	} else if errors.Is(workErr, context.DeadlineExceeded) {
		code, retry = "job_deadline", false
	} else if errors.Is(workErr, context.Canceled) {
		code, retry = "cancelled", true
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer stop()
	return true, errors.Join(heartbeatErr, s.Finish(cleanup, claim, code, retry))
}
