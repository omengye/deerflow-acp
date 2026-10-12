package runtime

import (
	"context"
	"fmt"
	"sync"
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
	mu      sync.Mutex
	waiting map[string]bool
}

type automaticSlotKey struct{}
type automaticSlot struct {
	queue *runQueue
	used  atomic.Bool
}

// Automatic work never queues ahead of a received user operation. The caller
// reserves a slot before a session and retains its release until cleanup joins.
func (q *runQueue) tryAutomatic(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if q.queued.Load() > 0 {
		return nil, nil, harness.ErrBusy
	}
	select {
	case q.slots <- struct{}{}:
		if ctx.Err() != nil || q.queued.Load() > 0 {
			<-q.slots
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, harness.ErrBusy
		}
		return context.WithValue(ctx, automaticSlotKey{}, &automaticSlot{queue: q}), func() { <-q.slots }, nil
	default:
		return nil, nil, harness.ErrBusy
	}
}

func newRunQueue(cfg RunQueueConfig) (*runQueue, error) {
	if cfg.MaxActiveRuns < 1 || cfg.MaxActiveRuns > 128 {
		return nil, fmt.Errorf("%w: max active runs must be 1..128", harness.ErrInvalidInput)
	}
	if cfg.QueueTimeout < 0 || cfg.QueueTimeout > harness.MaxExecutionTimeoutSeconds*time.Second {
		return nil, fmt.Errorf("%w: queue timeout must be 0 (unlimited) or at most %d seconds", harness.ErrInvalidInput, harness.MaxExecutionTimeoutSeconds)
	}
	return &runQueue{slots: make(chan struct{}, cfg.MaxActiveRuns), timeout: cfg.QueueTimeout, waiting: make(map[string]bool)}, nil
}

func (q *runQueue) isWaiting(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.waiting[id]
}

func (q *runQueue) acquire(ctx context.Context, sessionIDs ...string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if slot, ok := ctx.Value(automaticSlotKey{}).(*automaticSlot); ok && slot.queue == q {
		if !slot.used.CompareAndSwap(false, true) {
			return nil, harness.ErrBusy
		}
		return func() {}, nil
	}
	wait := ctx
	var cancel context.CancelFunc
	if q.timeout > 0 {
		wait, cancel = context.WithTimeout(ctx, q.timeout)
		defer cancel()
	}
	q.queued.Add(1)
	defer q.queued.Add(-1)
	if len(sessionIDs) > 0 {
		id := sessionIDs[0]
		q.mu.Lock()
		q.waiting[id] = true
		q.mu.Unlock()
		defer func() { q.mu.Lock(); delete(q.waiting, id); q.mu.Unlock() }()
	}
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
