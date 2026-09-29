package background

import (
	"context"
	"time"
)

// The private cancellation context leaves the native manager's heartbeat
// context alive while providers/tools and JoinAndClose finish. The monitor
// performs admission checks only; native heartbeat transactions renew leases.
func startBudgetMonitor(ctx context.Context, monitor BudgetMonitor, scope TaskScope, interval time.Duration) (context.Context, func() error) {
	child, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var failure error
		defer func() { done <- failure; close(done) }()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// An in-progress database probe must join before publishing an
				// outcome, including when the inner executor finishes meanwhile.
				probe, stopProbe := context.WithTimeout(context.WithoutCancel(ctx), max(interval, 5*time.Second))
				err := monitor.CheckAttemptBudget(probe, scope)
				stopProbe()
				if err != nil {
					failure = err
					cancel()
					return
				}
			}
		}
	}()
	return child, func() error { close(stop); err := <-done; cancel(); return err }
}
