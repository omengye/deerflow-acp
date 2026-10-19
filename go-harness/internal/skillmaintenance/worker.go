package skillmaintenance

import (
	"context"
	"errors"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

func heartbeatStoppedByWork(workErr, checkErr, err error) bool {
	if workErr == nil || checkErr != workErr {
		return false
	}
	// SQLite may return SQLITE_INTERRUPT instead of wrapping ctx.Err(). Only
	// accept it when the renewal inherited the worker's cancellation/deadline.
	// An independent renewal timeout or another storage error still fails closed.
	var code interface{ Code() int }
	return errors.Is(err, workErr) || errors.As(err, &code) && code.Code() == sqlite3.SQLITE_INTERRUPT
}

// Handler receives a host-owned claim, with the broker and fixed job deadline
// in its context. It must check producer input identity and source authority
// before model I/O, join cancellation, and never publish directly.
type Handler func(context.Context, Job) (code string, retry bool)

// RunOne claims at most one job and joins its heartbeat and handler. A host may
// drive a bounded worker loop; this package does not start a hidden scheduler.
func (s *Store) RunOne(ctx context.Context, handler Handler) (bool, error) {
	return s.RunKinds(ctx, []JobKind{SkillGeneration, SkillBehavior}, handler)
}

func (s *Store) RunKinds(ctx context.Context, kinds []JobKind, handler Handler) (bool, error) {
	if handler == nil {
		return false, ErrInvalid
	}
	claim, ok, err := s.ClaimKinds(ctx, kinds)
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
		leaseUntil := claim.leaseUntil
		for {
			select {
			case <-work.Done():
				done <- nil
				return
			case <-ticker.C:
				// Renewal shares the single SQLite connection with publication.
				// Wait within the confirmed lease, not an unrelated two-second
				// timer, and stop before expiry if it cannot be renewed. The final
				// lease inherits the job deadline so it remains a job_deadline.
				var check context.Context
				var stop context.CancelFunc
				if leaseUntil.Before(claim.Job.Deadline) {
					check, stop = context.WithDeadline(work, leaseUntil.Add(-2*time.Second))
				} else {
					check, stop = context.WithCancel(work)
				}
				renewedUntil, err := s.heartbeat(check, claim)
				checkErr := check.Err()
				stop()
				if err != nil {
					// RunKinds cancels work after the handler joins. An in-flight
					// renewal interrupted by that shutdown is not a storage fault.
					// A renewal's own deadline while work is live still fails closed.
					if heartbeatStoppedByWork(work.Err(), checkErr, err) {
						done <- nil
						return
					}
					cancel()
					done <- err
					return
				}
				leaseUntil = renewedUntil
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
