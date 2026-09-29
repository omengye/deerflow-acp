// Package mcp owns connection-scoped MCP resources and Eino tool adapters.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var ErrClosed = errors.New("MCP resources are closed")

type Manager struct {
	mu        sync.Mutex
	policy    harness.MCPPolicy
	allowed   map[string]os.FileInfo
	bindings  map[string]*binding
	pending   map[string]*binding
	all       map[*binding]struct{}
	retired   map[string]bool
	closed    bool
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

type binding struct {
	owner, id     string
	generation    string
	fingerprint   [32]byte
	ctx           context.Context
	cancel        context.CancelFunc
	ready, closed chan struct{}
	closeOnce     sync.Once
	closing       bool        // protected by Manager.mu
	err, closeErr error       // published by ready/closed respectively
	endpoints     []*endpoint // immutable after ready closes
}

func New(policy harness.MCPPolicy) (*Manager, error) {
	if policy.ConnectTimeout < 0 || policy.CallTimeout < 0 || policy.CloseTimeout < 0 || policy.MaxServers < 0 || policy.MaxTools < 0 || policy.MaxToolPages < 0 || policy.MaxResultChars < 0 {
		return nil, fmt.Errorf("%w: MCP policy limits cannot be negative", harness.ErrInvalidInput)
	}
	if policy.ConnectTimeout == 0 {
		policy.ConnectTimeout = 15 * time.Second
	}
	if policy.CallTimeout == 0 {
		policy.CallTimeout = 2 * time.Minute
	}
	if policy.CloseTimeout == 0 {
		policy.CloseTimeout = 5 * time.Second
	}
	if policy.MaxServers == 0 {
		policy.MaxServers = 8
	}
	if policy.MaxTools == 0 {
		policy.MaxTools = 128
	}
	if policy.MaxToolPages == 0 {
		policy.MaxToolPages = 20
	}
	if policy.MaxResultChars == 0 {
		policy.MaxResultChars = 64 << 10
	}
	m := &Manager{policy: policy, allowed: make(map[string]os.FileInfo), bindings: make(map[string]*binding), pending: make(map[string]*binding), all: make(map[*binding]struct{}), retired: make(map[string]bool), closeDone: make(chan struct{})}
	for _, command := range policy.AllowedCommands {
		path, info, err := executable(command)
		if err != nil {
			return nil, fmt.Errorf("%w: MCP allowlist entry is not an absolute executable", harness.ErrInvalidInput)
		}
		m.allowed[pathKey(path)] = info
	}
	// Keep no caller-owned slices/maps and do not retain redundant command config.
	m.policy.AllowedCommands = nil
	return m, nil
}

func (m *Manager) MCPCapabilities() (http, sse bool) { return m.policy.AllowHTTP, m.policy.AllowSSE }

// Bind is atomic: tools become visible only after all servers initialize and
// list successfully. Repeating an identical bind is a no-op. A replacement
// leaves the previous binding untouched until the candidate is ready. Runtime
// callers must hold the session's idle lifecycle lease while reconfiguring.
func (m *Manager) Bind(ctx context.Context, owner, id, cwd string, servers []harness.MCPServer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if owner == "" || id == "" {
		return fmt.Errorf("%w: MCP owner and session are required", harness.ErrInvalidInput)
	}
	prepared, realCWD, err := m.prepare(cwd, servers)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(struct {
		CWD     string
		Servers []harness.MCPServer
	}{realCWD, prepared})
	fingerprint := sha256.Sum256(data)
	m.mu.Lock()
	if m.closed || m.retired[owner] {
		m.mu.Unlock()
		return ErrClosed
	}
	if pending := m.pending[id]; pending != nil {
		if pending.owner != owner {
			m.mu.Unlock()
			return harness.ErrAttachedElsewhere
		}
		m.mu.Unlock()
		return harness.ErrBusy
	}
	previous := m.bindings[id]
	if existing := previous; existing != nil {
		if existing.owner != owner {
			m.mu.Unlock()
			return harness.ErrAttachedElsewhere
		}
		if existing.closing {
			m.mu.Unlock()
			return harness.ErrBusy
		}
		if existing.fingerprint == fingerprint {
			m.mu.Unlock()
			if existing.ctx.Err() != nil {
				return ErrClosed
			}
			return existing.err
		}
	}
	lifetime, cancel := context.WithCancel(context.Background())
	b := &binding{owner: owner, id: id, generation: rand.Text(), fingerprint: fingerprint, ctx: lifetime, cancel: cancel, ready: make(chan struct{}), closed: make(chan struct{})}
	m.pending[id] = b
	m.all[b] = struct{}{}
	m.mu.Unlock()
	toolCount := 0
	for _, server := range prepared {
		if err = ctx.Err(); err != nil {
			break
		}
		var e *endpoint
		e, err = m.connect(ctx, b, realCWD, server)
		if e != nil {
			b.endpoints = append(b.endpoints, e)
			toolCount += e.initialToolCount
		}
		if err != nil {
			break
		}
		if toolCount > m.policy.MaxTools {
			err = errors.New("MCP tool count exceeds configured limit")
			break
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && b.ctx.Err() != nil {
		err = ErrClosed
	}
	m.mu.Lock()
	if err == nil && (m.closed || m.retired[owner] || b.closing) {
		err = ErrClosed
	}
	if m.pending[id] == b {
		delete(m.pending, id)
	}
	if err == nil {
		m.bindings[id] = b
		if previous != nil {
			previous.closing = true
			previous.cancel()
		}
	}
	b.err = err
	close(b.ready)
	m.mu.Unlock()
	if err != nil {
		m.startClose(b)
		<-b.closed
		return errors.Join(err, b.closeErr)
	}
	if previous != nil {
		m.startClose(previous)
		<-previous.closed
		// Publication succeeded. Returning an error here could make the runtime
		// discard the live replacement; failed old cleanup stays tracked for Close.
	}
	return nil
}

func (m *Manager) Tools(ctx context.Context, id string) ([]tool.BaseTool, error) {
	m.mu.Lock()
	b := m.bindings[id]
	pending := m.pending[id]
	closed := m.closed
	if b != nil && b.closing {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	m.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if b == nil {
		if pending != nil {
			return nil, harness.ErrBusy
		}
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.ready:
	}
	if b.err != nil {
		return nil, b.err
	}
	if b.ctx.Err() != nil {
		return nil, ErrClosed
	}
	var result []tool.BaseTool
	for _, e := range b.endpoints {
		more, err := e.tools(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, more...)
		if len(result) > m.policy.MaxTools {
			return nil, fmt.Errorf("MCP tool count exceeds configured limit")
		}
	}
	sort.Slice(result, func(i, j int) bool { a, _ := result[i].Info(ctx); b, _ := result[j].Info(ctx); return a.Name < b.Name })
	return result, nil
}

// EnhancedTools projects MCP image blocks into Eino's native tool media path.
// The run's tool middleware imports them before returning a result to Eino.
func (m *Manager) EnhancedTools(ctx context.Context, id string) ([]tool.BaseTool, error) {
	base, err := m.Tools(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]tool.BaseTool, 0, len(base))
	for _, item := range base {
		guard, ok := item.(*guardTool)
		if !ok {
			return nil, errors.New("MCP tool adapter is invalid")
		}
		result = append(result, &enhancedGuardTool{guardTool: guard})
	}
	return result, nil
}

// Generation is an opaque connection identity without credentials. Callers
// hold the session lifecycle lease across Tools and Generation. Reconnecting
// an endpoint invalidates suspended execution even when its schema is equal.
// An empty binding has no remote resources and needs no process-local identity.
func (m *Manager) Generation(ctx context.Context, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", ErrClosed
	}
	b := m.bindings[id]
	if b == nil {
		if m.pending[id] != nil {
			return "", harness.ErrBusy
		}
		return "", nil
	}
	if b.closing || b.ctx.Err() != nil {
		return "", ErrClosed
	}
	if b.err != nil {
		return "", b.err
	}
	if len(b.endpoints) == 0 {
		return "", nil
	}
	return b.generation, nil
}

func (m *Manager) startClose(b *binding) {
	m.mu.Lock()
	b.closing = true
	b.cancel()
	m.mu.Unlock()
	b.closeOnce.Do(func() {
		go func() {
			<-b.ready
			var wg sync.WaitGroup
			errs := make(chan error, len(b.endpoints))
			for _, e := range b.endpoints {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := e.close(); err != nil {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				b.closeErr = errors.Join(b.closeErr, err)
			}
			m.mu.Lock()
			if m.bindings[b.id] == b {
				delete(m.bindings, b.id)
			}
			if b.closeErr == nil {
				delete(m.all, b)
			}
			m.mu.Unlock()
			close(b.closed)
		}()
	})
}

func (m *Manager) Release(ctx context.Context, owner, id string) error {
	m.mu.Lock()
	var owned []*binding
	for _, b := range []*binding{m.bindings[id], m.pending[id]} {
		if b != nil {
			if b.owner != owner {
				m.mu.Unlock()
				return harness.ErrAttachedElsewhere
			}
			owned = append(owned, b)
		}
	}
	// Reserve every captured generation before releasing the mutex, so a Bind
	// cannot publish a replacement in the gap before asynchronous cleanup starts.
	for _, b := range owned {
		b.closing = true
		b.cancel()
	}
	m.mu.Unlock()
	for _, b := range owned {
		m.startClose(b)
	}
	var result error
	for _, b := range owned {
		select {
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		case <-b.closed:
			result = errors.Join(result, b.closeErr)
		}
	}
	return result
}

func (m *Manager) ReleaseOwner(ctx context.Context, owner string) error {
	m.mu.Lock()
	m.retired[owner] = true
	var owned []*binding
	for b := range m.all {
		if b.owner == owner {
			owned = append(owned, b)
		}
	}
	m.mu.Unlock()
	for _, b := range owned {
		m.startClose(b)
	}
	var result error
	for _, b := range owned {
		select {
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		case <-b.closed:
			result = errors.Join(result, b.closeErr)
		}
	}
	return result
}

func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		var bindings []*binding
		for b := range m.all {
			bindings = append(bindings, b)
		}
		m.mu.Unlock()
		go func() {
			for _, b := range bindings {
				m.startClose(b)
			}
			for _, b := range bindings {
				<-b.closed // publishes the immutable endpoint list after setup
				for _, e := range b.endpoints {
					// Release's wait may have timed out. Global Close must still
					// join owned cleanup before the SDK can release runtime.lock.
					<-e.closed
					m.closeErr = errors.Join(m.closeErr, e.closeErr)
				}
				m.mu.Lock()
				delete(m.all, b)
				m.mu.Unlock()
			}
			close(m.closeDone)
		}()
	})
	return m.WaitClosed(context.Background())
}

// Done closes only after global Close has joined all locally owned transport
// cleanup and direct child processes. A Release timeout does not close Done.
func (m *Manager) Done() <-chan struct{} { return m.closeDone }

// WaitClosed waits for global Close to finish; it does not initiate shutdown.
// A context timeout stops only this wait. The caller must retain runtime/storage
// ownership until Done closes, even if an earlier Release reported an error.
func (m *Manager) WaitClosed(ctx context.Context) error {
	select {
	case <-m.closeDone:
		return m.closeErr
	default:
	}
	select {
	case <-m.closeDone:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
