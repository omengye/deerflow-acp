package session

import (
	"context"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type binding struct {
	owner   string
	busy    bool
	closing bool
	cancel  context.CancelFunc
	done    chan struct{}
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

func (c *Coordinator) Attach(id, owner string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

// Begin reserves the session synchronously, before dispatching work. Calling
// release is mandatory only after tool cleanup and final persistence complete.
func (c *Coordinator) Begin(parent context.Context, id, owner string) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	ctx, cancel := context.WithCancel(parent)
	b.busy, b.cancel, b.done = true, cancel, make(chan struct{})
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			c.mu.Lock()
			defer c.mu.Unlock()
			b.busy = false
			b.cancel = nil
			close(b.done)
			if b.closing && c.bindings[id] == b {
				delete(c.bindings, id)
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
		b.cancel()
	}
	return nil
}

func (c *Coordinator) Detach(ctx context.Context, id, owner string) error {
	c.mu.Lock()
	b := c.bindings[id]
	if b == nil || b.owner != owner {
		c.mu.Unlock()
		return harness.ErrNotAttached
	}
	b.closing = true
	var done <-chan struct{}
	if b.busy {
		b.cancel()
		done = b.done
	}
	c.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bindings[id] == b {
		delete(c.bindings, id)
	}
	return nil
}

func (c *Coordinator) Disconnect(ctx context.Context, owner string) error {
	c.mu.Lock()
	c.retired[owner] = true
	var ids []string
	for id, b := range c.bindings {
		if b.owner == owner {
			b.closing = true
			if !b.busy {
				delete(c.bindings, id)
				continue
			}
			ids = append(ids, id)
			if b.cancel != nil {
				b.cancel()
			}
		}
	}
	c.mu.Unlock()
	for _, id := range ids {
		if err := c.Detach(ctx, id, owner); err != nil && err != harness.ErrNotAttached {
			return err
		}
	}
	return nil
}
