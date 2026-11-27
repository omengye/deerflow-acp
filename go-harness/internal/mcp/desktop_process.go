package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// The binding owns the workers, including their final child waits. Only a
// request for one named server may create a worker.
type desktopProcesses struct {
	cwd     string
	servers []*desktopServer
	mu      sync.Mutex
	workers sync.WaitGroup
	closed  chan struct{}
}

type desktopServer struct {
	config   harness.MCPServer
	target   string
	saved    bool
	file     os.FileInfo
	cacheKey string
	cards    []CatalogTool
	complete bool
	bytes    int
	process  *desktopProcess
}

type desktopProcess struct {
	ctx         context.Context
	cancel      context.CancelFunc
	ready, done chan struct{}
	endpoint    *endpoint
	err         error
	refs        int // waiters, calls and execution leases
	timer       *time.Timer
	listMu      sync.Mutex
	paused      map[string]func(context.Context, string) bool
}

func (s *desktopServer) authorized(m *Manager) bool {
	if !s.saved {
		return m.hostOnly
	}
	grants, err := m.desktop.grants()
	g, ok := grants[s.config.Name]
	return err == nil && ok && g.matches(s.config) && pathKey(g.target) == pathKey(s.target)
}

func sameProgram(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Called under mu. Revocation or replacement immediately fences the old
// adapters, even when the settings change while a permission dialog is open.
func (s *desktopServer) check(m *Manager, cwd string) error {
	allowed := s.authorized(m)
	real, file, err := executable(s.config.Command)
	valid := err == nil && pathKey(real) == pathKey(s.target)
	if !s.saved && valid {
		valid = sameProgram(m.allowed[pathKey(real)], file)
	}
	if !allowed || !valid || !sameProgram(s.file, file) {
		if s.process != nil {
			s.process.cancel()
		}
		s.cards, s.complete, s.bytes, s.file, s.cacheKey = nil, false, 0, nil, ""
		if !allowed {
			return harness.ErrMCPLaunchDenied
		}
		if !valid {
			return &harness.MCPSetupError{Server: s.config.Name, Phase: "查找已授权的可执行文件"}
		}
		s.file = file
		s.cacheKey = s.catalogKey(cwd, file)
	}
	return nil
}

func (d *desktopProcesses) find(name string) *desktopServer {
	for _, s := range d.servers {
		if s.config.Name == name {
			return s
		}
	}
	return nil
}

// A nonempty expected generation may only borrow that live connection.
// After a cancelled startup or idle expiry, join cleanup before retrying.
func (d *desktopProcesses) acquire(ctx context.Context, m *Manager, b *binding, s *desktopServer, expected string) (*desktopProcess, error) {
	for {
		d.mu.Lock()
		if err := ctx.Err(); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		if b.ctx.Err() != nil {
			d.mu.Unlock()
			return nil, ErrClosed
		}
		if err := s.check(m, d.cwd); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		p := s.process
		if p != nil && p.endpoint != nil && p.endpoint.ctx.Err() != nil {
			p.cancel()
		}
		if expected != "" && (p == nil || p.ctx.Err() != nil || p.endpoint == nil || p.endpoint.desktopID != expected) {
			d.mu.Unlock()
			return nil, harness.ErrResourcesWaiting
		}
		if p != nil && p.ctx.Err() != nil {
			d.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-b.ctx.Done():
				return nil, ErrClosed
			case <-p.done:
			}
			continue
		}
		if p == nil {
			lifetime, cancel := context.WithCancel(b.ctx)
			p = &desktopProcess{ctx: lifetime, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
			s.process = p
			d.workers.Add(1)
			go d.run(m, b, s, p)
		}
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.refs++
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			d.release(s, p)
			return nil, ctx.Err()
		case <-b.ctx.Done():
			d.release(s, p)
			return nil, ErrClosed
		case <-p.ready:
			if p.err != nil {
				d.release(s, p)
				return nil, p.err
			}
			if p.ctx.Err() != nil {
				d.release(s, p)
				return nil, ErrClosed
			}
			return p, nil
		}
	}
}

func (d *desktopProcesses) release(s *desktopServer, p *desktopProcess) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p.refs--
	if p.refs != 0 || p.ctx.Err() != nil || p != s.process {
		return
	}
	if p.endpoint == nil {
		p.cancel()
		return
	}
	d.armIdle(s, p)
}

// A native permission interrupt releases the attempt lease while retaining
// its paused budget operation. Consult that operation before retiring its PID.
func (d *desktopProcesses) armIdle(s *desktopServer, p *desktopProcess) {
	if s.config.IdleTimeoutSeconds == 0 {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(time.Duration(s.config.IdleTimeoutSeconds)*time.Second, func() {
		d.mu.Lock()
		if s.process != p || p.timer != timer || p.refs != 0 || p.ctx.Err() != nil {
			d.mu.Unlock()
			return
		}
		paused := maps.Clone(p.paused)
		d.mu.Unlock()
		for id, check := range paused {
			if check(p.ctx, id) {
				continue
			}
			d.mu.Lock()
			delete(p.paused, id)
			d.mu.Unlock()
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if s.process != p || p.timer != timer || p.refs != 0 || p.ctx.Err() != nil {
			return
		}
		if len(p.paused) > 0 {
			d.armIdle(s, p)
			return
		}
		p.cancel()
	})
	p.timer = timer
}

func (t *leasedTool) RetainPausedOperation(id string, check func(context.Context, string) bool) {
	if t.server == nil || t.server.config.IdleTimeoutSeconds == 0 || id == "" {
		return
	}
	d := t.lease.binding.desktop
	d.mu.Lock()
	defer d.mu.Unlock()
	if t.process.paused == nil {
		t.process.paused = map[string]func(context.Context, string) bool{}
	}
	t.process.paused[id] = check
}

func (d *desktopProcesses) run(m *Manager, b *binding, s *desktopServer, p *desktopProcess) {
	defer d.workers.Done()
	config := s.config
	config.Command = s.target
	e, err := m.connect(p.ctx, b, d.cwd, config, rand.Text())
	defer func() {
		p.cancel()
		_ = e.close()
		<-e.closed
		d.mu.Lock()
		if p.timer != nil {
			p.timer.Stop()
		}
		if s.process == p {
			s.process = nil
		}
		close(p.done)
		d.mu.Unlock()
	}()
	if err == nil {
		err = d.publish(m, b, s, p, e, *e.desktopTools.Load())
	}
	d.mu.Lock()
	p.err = err
	if err == nil {
		p.endpoint = e
	} else {
		p.cancel()
	}
	close(p.ready)
	d.mu.Unlock()
	if err != nil {
		return
	}
	var checkGrant <-chan time.Time
	if s.saved {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		checkGrant = ticker.C
	}
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-e.ctx.Done():
			return
		case <-checkGrant:
			d.mu.Lock()
			_ = s.check(m, d.cwd)
			d.mu.Unlock()
		case <-e.listChanged:
			if d.relist(p.ctx, m, b, s, p, e) != nil {
				return
			}
		}
	}
}

func (d *desktopProcesses) relist(ctx context.Context, m *Manager, b *binding, s *desktopServer, p *desktopProcess, e *endpoint) error {
	p.listMu.Lock()
	defer p.listMu.Unlock()
	op, cancel := context.WithTimeout(ctx, m.policy.ConnectTimeout)
	defer cancel()
	items, err := e.tools(op)
	if err != nil {
		return err
	}
	return d.publish(m, b, s, p, e, items)
}

func (d *desktopProcesses) publish(m *Manager, b *binding, s *desktopServer, p *desktopProcess, e *endpoint, items []tool.BaseTool) error {
	cards, size, err := catalogTools(items)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if err = s.check(m, d.cwd); err != nil || p.ctx.Err() != nil || s.process != p {
		d.mu.Unlock()
		return ErrClosed
	}
	count, total := len(items), size
	for _, sibling := range d.servers {
		if sibling != s {
			count += len(sibling.cards)
			total += sibling.bytes
		}
	}
	for _, bound := range b.endpoints {
		count += bound.initialToolCount
		total += bound.initialToolBytes
	}
	if count > m.policy.MaxTools || total > maxCatalogBytes {
		d.mu.Unlock()
		return errors.New("MCP catalog exceeds configured limits")
	}
	s.cards, s.complete, s.bytes = cards, true, size
	e.desktopTools.Store(&items)
	key := s.cacheKey
	d.mu.Unlock()
	m.saveCatalog(p.ctx, key, cards)
	return nil
}

func (d *desktopProcesses) close(timeout time.Duration) error {
	d.mu.Lock()
	d.mu.Unlock() // synchronize the final worker admission with the join
	go func() { d.workers.Wait(); close(d.closed) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-d.closed:
		return nil
	case <-timer.C:
		return errors.New("desktop MCP cleanup timed out")
	}
}
