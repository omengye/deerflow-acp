package launch

import (
	"context"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// StartRetention runs an immediate sweep and then periodic sweeps until stop
// joins the worker. Both stdio and daemon hosts call stop before Client.Close.
func StartRetention(parent context.Context, client *deerflow.Client, policy harness.RetentionPolicy, report func(error)) func() {
	if !policy.Enabled {
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(policy.CheckInterval)
		defer ticker.Stop()
		for {
			sweepCtx, stopSweep := context.WithTimeout(ctx, 10*time.Minute)
			_, err := client.CleanupExpiredSessions(sweepCtx)
			stopSweep()
			if err != nil && ctx.Err() == nil && report != nil {
				report(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
