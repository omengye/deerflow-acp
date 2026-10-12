package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Describe returns stable resource identity and the current, ephemeral binding
// generation. It grants no authority to use a replacement connection.
func (m *Manager) Describe(ctx context.Context, id string) (harness.MCPResourceDescriptor, string, error) {
	if host, recipe := m.hostBinding(id); host != nil {
		return host.Describe(ctx, recipe)
	}
	descriptor, b, _, err := m.resourceSnapshot(ctx, id)
	if err != nil || b == nil || len(b.endpoints) == 0 {
		return descriptor, "", err
	}
	return descriptor, b.generation, nil
}

// resourceSnapshot discovers once and fingerprints exactly those adapters. A
// concurrent replacement cannot splice one binding's identity into another's
// tool set. No connection configuration is returned to callers.
func (m *Manager) resourceSnapshot(ctx context.Context, id string) (harness.MCPResourceDescriptor, *binding, []*guardTool, error) {
	var empty harness.MCPResourceDescriptor
	if err := ctx.Err(); err != nil {
		return empty, nil, nil, err
	}
	m.mu.Lock()
	b, pending, closed := m.bindings[id], m.pending[id], m.closed
	invalid := b != nil && (b.closing || b.ctx.Err() != nil)
	m.mu.Unlock()
	if closed || invalid {
		return empty, nil, nil, ErrClosed
	}
	if b == nil {
		if pending != nil {
			return empty, nil, nil, harness.ErrBusy
		}
		return empty, nil, nil, nil
	}
	select {
	case <-ctx.Done():
		return empty, nil, nil, ctx.Err()
	case <-b.ready:
	}
	if b.err != nil {
		return empty, nil, nil, b.err
	}
	if len(b.endpoints) == 0 {
		return empty, b, nil, nil
	}
	descriptor := harness.MCPResourceDescriptor{Version: 1, ConfigDigest: hex.EncodeToString(b.fingerprint[:]), Servers: make([]string, 0, len(b.endpoints)), Tools: []string{}}
	var adapters []*guardTool
	for _, e := range b.endpoints {
		descriptor.Servers = append(descriptor.Servers, e.name)
		items, err := e.tools(ctx)
		if err != nil {
			return empty, nil, nil, err
		}
		for _, item := range items {
			adapter, ok := item.(*guardTool)
			if !ok {
				return empty, nil, nil, errors.New("MCP tool adapter is invalid")
			}
			adapters = append(adapters, adapter)
		}
		if len(adapters) > m.policy.MaxTools {
			return empty, nil, nil, errors.New("MCP tool count exceeds configured limit")
		}
	}
	type entry struct {
		Name    string
		Info    json.RawMessage
		Adapter *guardTool `json:"-"`
	}
	entries := make([]entry, 0, len(adapters))
	for _, adapter := range adapters {
		info, err := adapter.Info(ctx)
		if err != nil || info == nil {
			return empty, nil, nil, errors.New("MCP tool schema is invalid")
		}
		data, err := canonicalToolInfo(info)
		if err != nil {
			return empty, nil, nil, errors.New("MCP tool schema is invalid")
		}
		entries = append(entries, entry{Name: info.Name, Info: data, Adapter: adapter})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	sort.Strings(descriptor.Servers)
	for i, item := range entries {
		if i > 0 && entries[i-1].Name == item.Name {
			return empty, nil, nil, errors.New("MCP tool names are not unique")
		}
		descriptor.Tools = append(descriptor.Tools, item.Name)
		adapters[i] = item.Adapter
	}
	data, err := json.Marshal(struct {
		Servers []string
		Tools   []entry
	}{descriptor.Servers, entries})
	if err != nil {
		return empty, nil, nil, errors.New("MCP resource schema is invalid")
	}
	digest := sha256.Sum256(data)
	descriptor.SchemaDigest = hex.EncodeToString(digest[:])
	m.mu.Lock()
	valid := !m.closed && m.bindings[id] == b && !b.closing && b.ctx.Err() == nil
	m.mu.Unlock()
	if !valid {
		return empty, nil, nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return empty, nil, nil, err
	}
	return descriptor, b, adapters, nil
}

// Lease borrows tools without owning the parent session's connection. Close
// cancels only this borrower. Done also closes on binding or transport loss.
// The returned Tools slice must be treated as read-only.
type Lease struct {
	Tools   []tool.BaseTool
	Done    <-chan struct{}
	manager *Manager
	binding *binding
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
}

func (m *Manager) Acquire(ctx context.Context, id string, expected harness.MCPResourceDescriptor, generation string) (*Lease, error) {
	if host, recipe := m.hostBinding(id); host != nil {
		return host.Acquire(ctx, recipe, expected, generation)
	}
	if expected.Version != 1 || expected.ConfigDigest == "" || expected.SchemaDigest == "" || generation == "" {
		return nil, harness.ErrResourcesIncompatible
	}
	actual, b, adapters, err := m.resourceSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	if b == nil || len(b.endpoints) == 0 || b.generation != generation {
		return nil, harness.ErrResourcesWaiting
	}
	if actual.Version != expected.Version || actual.ConfigDigest != expected.ConfigDigest || actual.SchemaDigest != expected.SchemaDigest || !slices.Equal(actual.Servers, expected.Servers) || !slices.Equal(actual.Tools, expected.Tools) {
		return nil, harness.ErrResourcesIncompatible
	}
	lifetime, cancel := context.WithCancel(b.ctx)
	lease := &Lease{manager: m, binding: b, ctx: lifetime, cancel: cancel, Done: lifetime.Done(), Tools: make([]tool.BaseTool, 0, len(adapters))}
	for _, adapter := range adapters {
		lease.Tools = append(lease.Tools, &leasedTool{lease: lease, raw: &enhancedGuardTool{guardTool: adapter}})
	}
	if err := lease.Check(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	return lease, nil
}

func (l *Lease) Check() error {
	if l == nil || l.ctx.Err() != nil {
		return ErrClosed
	}
	l.manager.mu.Lock()
	defer l.manager.mu.Unlock()
	if l.manager.closed || l.manager.bindings[l.binding.id] != l.binding || l.binding.closing || l.binding.ctx.Err() != nil {
		return ErrClosed
	}
	return nil
}

func (l *Lease) Close() error {
	if l != nil {
		l.once.Do(l.cancel)
	}
	return nil
}

type leasedTool struct {
	lease *Lease
	raw   *enhancedGuardTool
}

func (t *leasedTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return t.raw.Info(ctx) }

func (t *leasedTool) InvokableRun(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
	if err := t.lease.Check(); err != nil {
		return nil, harness.MarkToolNoEffect(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, harness.MarkToolNoEffect(err)
	}
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.lease.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := t.lease.Check(); err != nil {
		return nil, harness.MarkToolNoEffect(err)
	}
	// Once dispatched, connection errors remain uncertain; never retry or
	// relabel an external effect just because the borrower's lease was lost.
	return t.raw.InvokableRun(op, args, opts...)
}

var _ tool.EnhancedInvokableTool = (*leasedTool)(nil)
