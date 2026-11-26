package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Each saved program owns its transport. Foreground turns sample the ready
// connections, while a slow handshake can finish independently of the chat.
type desktopProcesses struct {
	cwd              string
	servers          []desktopServer
	published        []*endpoint
	generationCtx    context.Context
	retireGeneration context.CancelFunc
	mu               sync.Mutex
	workers          sync.WaitGroup
	closed           chan struct{}
}

type desktopServer struct {
	config  harness.MCPServer
	target  string
	process *desktopProcess
}

type desktopProcess struct {
	file     os.FileInfo
	ctx      context.Context
	cancel   context.CancelFunc
	ready    chan struct{}
	endpoint *endpoint // published by closing ready
}

func (d *desktopProcesses) refresh(ctx context.Context, m *Manager, b *binding) {
	grants, _ := m.desktop.grants() // an unreadable or revoked grant cannot launch a replacement
	d.mu.Lock()
	if b.ctx.Err() != nil || ctx.Err() != nil {
		d.mu.Unlock()
		return
	}
	var started []<-chan struct{}
	for i := range d.servers {
		s := &d.servers[i]
		real, info, err := executable(s.config.Command)
		grant, authorized := grants[s.config.Name]
		available := authorized && grant.matches(s.config) && err == nil && pathKey(real) == pathKey(s.target)
		if old := s.process; old != nil {
			if available && old.ctx.Err() == nil && os.SameFile(old.file, info) &&
				old.file.Size() == info.Size() && old.file.ModTime().Equal(info.ModTime()) {
				continue
			}
			old.cancel()
			s.process = nil
		}
		if !available {
			continue
		}
		lifetime, cancel := context.WithCancel(b.ctx)
		p := &desktopProcess{file: info, ctx: lifetime, cancel: cancel, ready: make(chan struct{})}
		s.process = p
		config := s.config
		config.Command = real
		started = append(started, p.ready)
		d.workers.Add(1)
		go func() {
			defer d.workers.Done()
			p.run(m, b, d.cwd, config)
		}()
	}
	d.mu.Unlock()
	defer d.publishReady(b.ctx)

	// One shared startup allowance for all new programs. A pending handshake
	// keeps its normal connect timeout and is not restarted on the next prompt.
	if len(started) > 0 {
		timer := time.NewTimer(1500 * time.Millisecond)
		defer timer.Stop()
		for _, ready := range started {
			select {
			case <-ready:
			case <-timer.C:
				return
			case <-ctx.Done():
				return
			case <-b.ctx.Done():
				return
			}
		}
	}
}

func (p *desktopProcess) run(m *Manager, b *binding, cwd string, cfg harness.MCPServer) {
	e, err := m.connect(p.ctx, b, cwd, cfg, rand.Text())
	if err == nil {
		p.endpoint = e
	} else {
		if p.ctx.Err() == nil {
			slog.Warn("Desktop MCP server unavailable", "server", cfg.Name)
		}
		p.cancel()
	}
	close(p.ready)
	defer func() {
		p.cancel()
		_ = e.close()
		<-e.closed // includes reaping the child, even after a cleanup timeout
	}()
	if err != nil {
		return
	}
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-e.ctx.Done():
			return
		case <-e.listChanged:
			op, cancel := context.WithTimeout(p.ctx, m.policy.ConnectTimeout)
			items, listErr := e.tools(op)
			cancel()
			if listErr != nil {
				return
			}
			e.desktopTools.Store(&items)
		}
	}
}

// Publish slow startups only at a turn or session boundary. A checkpoint in
// the current turn must keep the set that was available when it was admitted.
func (d *desktopProcesses) publishReady(parent context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ready []*endpoint
	for _, s := range d.servers {
		p := s.process
		if p == nil || p.ctx.Err() != nil {
			continue
		}
		select {
		case <-p.ready:
			if e := p.endpoint; e != nil && e.ctx.Err() == nil {
				ready = append(ready, e)
			}
		default:
		}
	}
	if d.generationCtx == nil || !slices.Equal(ready, d.published) {
		if d.retireGeneration != nil {
			d.retireGeneration()
		}
		d.generationCtx, d.retireGeneration = context.WithCancel(parent)
		d.published = ready
	}
}

func (d *desktopProcesses) active() ([]*endpoint, context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var active []*endpoint
	for _, e := range d.published {
		if e.ctx.Err() == nil {
			active = append(active, e)
		}
	}
	return active, d.generationCtx
}

// The binding has been cancelled. Synchronize with the final worker admission
// before waiting, and let global Close join even after a bounded Release.
func (d *desktopProcesses) close(timeout time.Duration) error {
	d.mu.Lock()
	d.mu.Unlock()
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
