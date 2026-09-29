package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var (
	ErrExplicitCancel = errors.New("execution explicitly cancelled")
	ErrDisconnected   = errors.New("execution owner disconnected")
)

type binding struct {
	owner        string
	busy         bool
	closing      bool
	cancel       context.CancelCauseFunc
	done         chan struct{}
	cleanup      func(context.Context) error
	closeAttempt *closeAttempt
}

type closeAttempt struct {
	done chan struct{}
	err  error
}

// Coordinator holds process-local ownership. A separate database ownership lock
// is required when hosting the service in more than one executable.
type Coordinator struct {
	mu       sync.Mutex
	bindings map[string]*binding
	retired  map[string]bool
}

func NewCoordinator() *Coordinator {
	return &Coordinator{bindings: make(map[string]*binding), retired: make(map[string]bool)}
}

// Authorize checks the current connection generation without reserving a new
// foreground turn. Background tools call this while their parent owns the
// existing turn lease. Closing or retired owners cannot start task operations.
func (c *Coordinator) Authorize(id, owner string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bindings[id]
	if owner == "" || c.retired[owner] || b == nil || b.owner != owner || b.closing {
		return harness.ErrNotAttached
	}
	return nil
}

func (c *Coordinator) Attach(id, owner string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attachLocked(id, owner)
}

func (c *Coordinator) attachLocked(id, owner string) (bool, error) {
	if c.retired[owner] {
		return false, harness.ErrNotAttached
	}
	if b := c.bindings[id]; b != nil {
		if b.owner != owner {
			return false, harness.ErrAttachedElsewhere
		}
		if b.closing || b.busy {
			return false, harness.ErrBusy
		}
		return false, nil
	}
	c.bindings[id] = &binding{owner: owner}
	return true, nil
}

// AttachAndBegin atomically reserves a newly attached or idle session before
// resources are bound or history is replayed. No prompt can enter that gap.
func (c *Coordinator) AttachAndBegin(parent context.Context, id, owner string) (context.Context, func(), bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh, err := c.attachLocked(id, owner)
	if err != nil {
		return nil, nil, false, err
	}
	ctx, release, err := c.beginLocked(parent, id, owner)
	return ctx, release, fresh, err
}

// Begin reserves the session synchronously, before dispatching work. Calling
// release is mandatory only after tool cleanup and final persistence complete.
func (c *Coordinator) Begin(parent context.Context, id, owner string) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.beginLocked(parent, id, owner)
}

func (c *Coordinator) beginLocked(parent context.Context, id, owner string) (context.Context, func(), error) {
	if c.retired[owner] {
		return nil, nil, harness.ErrNotAttached
	}
	b := c.bindings[id]
	if b == nil || b.owner != owner {
		return nil, nil, harness.ErrNotAttached
	}
	if b.busy || b.closing {
		return nil, nil, harness.ErrBusy
	}
	ctx, cancel := context.WithCancelCause(parent)
	b.busy, b.cancel, b.done = true, cancel, make(chan struct{})
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel(nil)
			c.mu.Lock()
			defer c.mu.Unlock()
			b.busy = false
			b.cancel = nil
			close(b.done)
			if b.closing && b.cleanup == nil && b.closeAttempt != nil {
				c.finishCloseLocked(id, b, b.closeAttempt, nil)
			}
		})
	}, nil
}

func (c *Coordinator) Cancel(id, owner string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bindings[id]
	if b == nil || b.owner != owner {
		return harness.ErrNotAttached
	}
	if b.cancel != nil {
		b.cancel(ErrExplicitCancel)
	}
	return nil
}

func (c *Coordinator) Detach(ctx context.Context, id, owner string) error {
	return c.DetachWithCleanup(ctx, id, owner, nil)
}

// DetachWithCleanup retains ownership until both the run and its resources are
// closed. Caller cancellation stops waiting; it does not abandon cleanup. Failed
// resource cleanup keeps the session closing and can be retried by its owner.
func (c *Coordinator) DetachWithCleanup(ctx context.Context, id, owner string, cleanup func(context.Context) error) error {
	c.mu.Lock()
	b := c.bindings[id]
	if b == nil || b.owner != owner {
		c.mu.Unlock()
		return harness.ErrNotAttached
	}
	attempt, work := c.startCloseLocked(id, b, cleanup)
	c.mu.Unlock()
	if work != nil {
		go work()
	}
	return awaitClose(ctx, attempt)
}

func (c *Coordinator) Disconnect(ctx context.Context, owner string) error {
	return c.DisconnectWithCleanup(ctx, owner, nil)
}

func (c *Coordinator) DisconnectWithCleanup(ctx context.Context, owner string, cleanup func(context.Context, string) error) error {
	c.mu.Lock()
	c.retired[owner] = true
	var attempts []*closeAttempt
	var workers []func()
	for id, b := range c.bindings {
		if b.owner == owner {
			var fn func(context.Context) error
			if cleanup != nil {
				fn = func(ctx context.Context) error { return cleanup(ctx, id) }
			}
			attempt, work := c.startCloseLocked(id, b, fn)
			attempts = append(attempts, attempt)
			if work != nil {
				workers = append(workers, work)
			}
		}
	}
	c.mu.Unlock()
	for _, work := range workers {
		go work()
	}
	var joined error
	for _, attempt := range attempts {
		joined = errors.Join(joined, awaitClose(ctx, attempt))
	}
	return joined
}

func (c *Coordinator) startCloseLocked(id string, b *binding, cleanup func(context.Context) error) (*closeAttempt, func()) {
	if previous := b.closeAttempt; previous != nil {
		select {
		case <-previous.done:
			if previous.err == nil {
				return previous, nil
			}
		default:
			return previous, nil
		}
	}
	attempt := &closeAttempt{done: make(chan struct{})}
	b.closing, b.cleanup, b.closeAttempt = true, cleanup, attempt
	var runDone <-chan struct{}
	if b.busy {
		b.cancel(ErrDisconnected)
		runDone = b.done
	}
	if cleanup == nil {
		if !b.busy {
			c.finishCloseLocked(id, b, attempt, nil)
		}
		return attempt, nil
	}
	return attempt, func() {
		if runDone != nil {
			<-runDone
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := cleanup(cleanupCtx)
		cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.finishCloseLocked(id, b, attempt, err)
	}
}

func (c *Coordinator) finishCloseLocked(id string, b *binding, attempt *closeAttempt, err error) {
	attempt.err = err
	if err == nil && c.bindings[id] == b {
		delete(c.bindings, id)
	}
	close(attempt.done)
}

func awaitClose(ctx context.Context, attempt *closeAttempt) error {
	select {
	case <-attempt.done:
		return attempt.err
	default:
	}
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
