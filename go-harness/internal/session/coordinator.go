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
	maintenance  bool
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
	cleanups map[string]bool
	retired  map[string]bool
	draining bool
}

// Activity is a point-in-time view for the authenticated local management
// channel. It intentionally excludes connection owner identities.
type Activity struct {
	Draining         bool
	ActiveOperations int
	Phases           map[string]string
}

func (c *Coordinator) SetDraining(value bool) Activity {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.draining = value
	return c.activityLocked()
}

func (c *Coordinator) Activity() Activity {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activityLocked()
}

func (c *Coordinator) activityLocked() Activity {
	out := Activity{Draining: c.draining, Phases: make(map[string]string, len(c.bindings)+len(c.cleanups))}
	for id := range c.cleanups {
		out.Phases[id] = "deleting"
		out.ActiveOperations++
	}
	for id, b := range c.bindings {
		switch {
		case b.closing:
			out.Phases[id] = "closing"
		case b.busy:
			out.Phases[id] = "running"
		default:
			out.Phases[id] = "attached"
		}
		if b.busy {
			out.ActiveOperations++
		}
	}
	return out
}

func NewCoordinator() *Coordinator {
	return &Coordinator{bindings: make(map[string]*binding), cleanups: make(map[string]bool), retired: make(map[string]bool)}
}

// ReserveCleanup fences Attach for the whole purge, including file cleanup
// after the database transaction. It succeeds only for a detached session.
func (c *Coordinator) ReserveCleanup(id string) (func(), error) {
	if id == "" {
		return nil, harness.ErrInvalidInput
	}
	c.mu.Lock()
	if c.bindings[id] != nil || c.cleanups[id] {
		c.mu.Unlock()
		return nil, harness.ErrBusy
	}
	c.cleanups[id] = true
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.cleanups, id)
			c.mu.Unlock()
		})
	}, nil
}

// ReserveOwnerCleanup atomically replaces an idle owner's attachment with a
// deletion fence after periodic housekeeping releases it. finish(true) restores
// a closing attachment if resource cleanup fails, fencing prompts until retry.
// finish(false) leaves it detached after resource release or purge.
func (c *Coordinator) ReserveOwnerCleanup(ctx context.Context, id, owner string) (func(bool), error) {
	c.mu.Lock()
	if b := c.bindings[id]; b != nil && b.owner == owner && b.maintenance {
		if err := c.awaitMaintenanceLocked(ctx, id); err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
	if c.cleanups[id] {
		c.mu.Unlock()
		return nil, harness.ErrBusy
	}
	b := c.bindings[id]
	if b == nil {
		c.mu.Unlock()
		return nil, harness.ErrNotAttached
	}
	if b.owner != owner {
		c.mu.Unlock()
		return nil, harness.ErrAttachedElsewhere
	}
	if b.busy || b.closing {
		c.mu.Unlock()
		return nil, harness.ErrBusy
	}
	delete(c.bindings, id)
	c.cleanups[id] = true
	c.mu.Unlock()
	var once sync.Once
	return func(restore bool) {
		once.Do(func() {
			c.mu.Lock()
			if restore && !c.retired[owner] {
				b.closing = true
				c.bindings[id] = b
			}
			delete(c.cleanups, id)
			c.mu.Unlock()
		})
	}, nil
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
	// Housekeeping holds the session for one bounded transaction only.
	if err := c.awaitMaintenanceLocked(context.Background(), id); err != nil {
		return false, err
	}
	return c.attachLocked(id, owner)
}

func (c *Coordinator) attachLocked(id, owner string) (bool, error) {
	if c.draining || c.cleanups[id] {
		return false, harness.ErrBusy
	}
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
	if err := c.awaitMaintenanceLocked(parent, id); err != nil {
		return nil, nil, false, err
	}
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
	if err := c.awaitMaintenanceLocked(parent, id); err != nil {
		return nil, nil, err
	}
	return c.beginLocked(parent, id, owner)
}

// BeginMaintenance reserves an idle session for short periodic housekeeping
// (a single bounded transaction). Housekeeping yields to real work: Begin,
// AttachAndBegin and CancelOrBegin wait for it to release instead of failing
// with ErrBusy or mistaking it for an active run to cancel.
func (c *Coordinator) BeginMaintenance(parent context.Context, id, owner string) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, release, err := c.beginLocked(parent, id, owner)
	if err == nil {
		c.bindings[id].maintenance = true
	}
	return ctx, release, err
}

// awaitMaintenanceLocked waits, with c.mu held on entry and return, until the
// session is not reserved by housekeeping.
func (c *Coordinator) awaitMaintenanceLocked(parent context.Context, id string) error {
	for {
		b := c.bindings[id]
		if b == nil || !b.busy || !b.maintenance {
			return nil
		}
		done := b.done
		c.mu.Unlock()
		select {
		case <-done:
			c.mu.Lock()
		case <-parent.Done():
			c.mu.Lock()
			return context.Cause(parent)
		}
	}
}

func (c *Coordinator) beginLocked(parent context.Context, id, owner string) (context.Context, func(), error) {
	if c.draining {
		return nil, nil, harness.ErrBusy
	}
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
			b.busy, b.maintenance = false, false
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

// CancelOrBegin signals an active operation, or reserves an idle session so
// the runtime can cancel a saved wait and persist its stop fence. A later
// user/automatic operation cannot enter before idle cancellation commits.
// A nonnil release must be called after persistence; never wait for active I/O.
func (c *Coordinator) CancelOrBegin(ctx context.Context, id, owner string) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bindings[id]
	if b == nil || b.owner != owner || c.retired[owner] || b.closing {
		return nil, nil, harness.ErrNotAttached
	}
	if err := c.awaitMaintenanceLocked(ctx, id); err != nil {
		return nil, nil, err
	}
	if b = c.bindings[id]; b == nil || b.owner != owner || c.retired[owner] || b.closing {
		return nil, nil, harness.ErrNotAttached
	}
	if b.busy {
		if b.cancel != nil {
			b.cancel(ErrExplicitCancel)
		}
		return nil, nil, nil
	}
	return c.beginLocked(ctx, id, owner)
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
