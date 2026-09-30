package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// RunQueueConfig limits foreground model executions across all sessions in a
// Service. QueueTimeout does not consume the run's execution-time budget; zero
// disables the queue deadline.
type RunQueueConfig struct {
	MaxActiveRuns int
	QueueTimeout  time.Duration
}

type runQueue struct {
	slots   chan struct{}
	timeout time.Duration
	queued  atomic.Int64
}

func newRunQueue(cfg RunQueueConfig) (*runQueue, error) {
	if cfg.MaxActiveRuns < 1 || cfg.MaxActiveRuns > 128 {
		return nil, fmt.Errorf("%w: max active runs must be 1..128", harness.ErrInvalidInput)
	}
	if cfg.QueueTimeout < 0 || cfg.QueueTimeout > 24*time.Hour {
		return nil, fmt.Errorf("%w: queue timeout must be 0..24h", harness.ErrInvalidInput)
	}
	return &runQueue{slots: make(chan struct{}, cfg.MaxActiveRuns), timeout: cfg.QueueTimeout}, nil
}

func (q *runQueue) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wait := ctx
	var cancel context.CancelFunc
	if q.timeout > 0 {
		wait, cancel = context.WithTimeout(ctx, q.timeout)
		defer cancel()
	}
	q.queued.Add(1)
	defer q.queued.Add(-1)
	select {
	case q.slots <- struct{}{}:
		// If cancellation raced with an available slot, do not start a run.
		if err := ctx.Err(); err != nil {
			<-q.slots
			return nil, err
		}
		if wait.Err() != nil {
			<-q.slots
			return nil, harness.ErrQueueTimeout
		}
		return func() { <-q.slots }, nil
	case <-wait.Done():
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, harness.ErrQueueTimeout
	}
}
