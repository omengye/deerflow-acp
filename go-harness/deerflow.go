// Package deerflow assembles the embeddable harness and local ACP service.
package deerflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/agent"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

type Config struct {
	DataDir       string
	Provider      string
	APIKey        string
	BaseURL       string
	Model         string
	Instruction   string
	MaxIterations int
	// Models allow session selection within the configured provider. Model is
	// always included. Provider credentials never become session configuration.
	Models           []harness.ConfigValue
	DisableSubagents bool
	// Nil uses DefaultBudgetLimits. A non-nil zero value disables all quotas.
	// Token accounting is estimated until the provider reports actual usage.
	Budget *harness.BudgetLimits
	MCP    harness.MCPPolicy
	// Engine allows embedding a custom execution backend without importing Eino.
	// When nil, the real Eino DeepAgent and durable SQLite stores are used.
	Engine harness.Engine
}

type Client struct {
	service    *hr.Service
	store      *sqlite.Store
	mcp        *mcp.Manager
	lock       *flock.Flock
	owner      string
	mu         sync.Mutex
	closed     bool
	agents     map[*agent.Agent]struct{}
	operations sync.WaitGroup
	closeDone  chan struct{}
	closeErr   error
}

func Open(ctx context.Context, cfg Config) (client *Client, err error) {
	if cfg.DataDir == "" {
		base, e := os.UserConfigDir()
		if e != nil {
			return nil, e
		}
		cfg.DataDir = filepath.Join(base, "deerflow-go")
	}
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, err
	}
	// Resolve aliases before locking, so symlink spellings cannot acquire two
	// independent lock paths for one database. The OS releases locks on crashes.
	cfg.DataDir, err = filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(cfg.DataDir, "runtime.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, fmt.Errorf("data directory already has a running harness: %s", cfg.DataDir)
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	store, err := sqlite.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = store.Close()
		}
	}()
	business, err := hr.NewStore(ctx, store.DB())
	if err != nil {
		return nil, err
	}
	if err = business.ReconcileInterrupted(ctx); err != nil {
		return nil, err
	}
	manager, err := mcp.New(cfg.MCP)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = manager.Close()
		}
	}()
	engine := cfg.Engine
	if engine == nil {
		budget := harness.DefaultBudgetLimits()
		if cfg.Budget != nil {
			budget = *cfg.Budget
		}
		factory := func(ctx context.Context, req harness.RunRequest) ([]tool.BaseTool, func() error, error) {
			readOnly := req.Session.Mode == "plan" || req.Session.ApprovalMode == harness.ApprovalReadOnly
			if readOnly {
				req.Session.Mode = "plan"
			}
			local, cleanup, err := tools.WorkspaceFactory(ctx, req)
			if err != nil || readOnly {
				return local, cleanup, err
			}
			remote, err := manager.Tools(ctx, req.Session.ID)
			return append(local, remote...), cleanup, err
		}
		engine, err = einoengine.New(ctx, einoengine.Config{Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model, Instruction: cfg.Instruction, MaxIterations: cfg.MaxIterations, Budget: budget, DisableSubAgent: cfg.DisableSubagents, CheckpointStore: store, SessionStore: store, ToolFactory: factory})
		if err != nil {
			return nil, err
		}
	}
	service := hr.NewService(business, engine, cfg.Model)
	service.Resources = manager
	service.Settings = hr.ConfigSettings{Models: append([]harness.ConfigValue(nil), cfg.Models...), EnableSubagents: !cfg.DisableSubagents, DefaultSubagents: !cfg.DisableSubagents}
	return &Client{service: service, store: store, mcp: manager, lock: lock, owner: hr.NewID(), agents: make(map[*agent.Agent]struct{}), closeDone: make(chan struct{})}, nil
}

func (c *Client) operation() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("harness is closed")
	}
	c.operations.Add(1)
	return c.operations.Done, nil
}
func (c *Client) NewSession(ctx context.Context, cwd string, servers ...harness.MCPServer) (harness.Session, error) {
	done, err := c.operation()
	if err != nil {
		return harness.Session{}, err
	}
	defer done()
	return c.service.NewSession(ctx, c.owner, cwd, servers...)
}
func (c *Client) LoadSession(ctx context.Context, id, cwd string, replay bool, emit harness.EventHandler, servers ...harness.MCPServer) (harness.Session, error) {
	done, err := c.operation()
	if err != nil {
		return harness.Session{}, err
	}
	defer done()
	return c.service.Load(ctx, c.owner, id, cwd, replay, emit, servers...)
}

func (c *Client) SetConfigOption(ctx context.Context, id, key, value string) ([]harness.ConfigOption, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.service.SetConfigOption(ctx, c.owner, id, key, value)
}

func (c *Client) ConfigOptions(ctx context.Context, id string) ([]harness.ConfigOption, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	ctx, release, err := c.service.Coordinator.Begin(ctx, id, c.owner)
	if err != nil {
		return nil, err
	}
	defer release()
	x, err := c.service.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	return c.service.ConfigOptions(x), nil
}

func (c *Client) SetMode(ctx context.Context, id, mode string) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.service.SetMode(ctx, c.owner, id, mode)
}
func (c *Client) Run(ctx context.Context, id string, input []harness.Content, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.RunResult{}, err
	}
	defer done()
	return c.service.Run(ctx, c.owner, id, input, emit, approve)
}
func (c *Client) Cancel(id string) error { return c.service.Coordinator.Cancel(id, c.owner) }
func (c *Client) CloseSession(ctx context.Context, id string) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.service.CloseSession(ctx, c.owner, id)
}

// ServeACP owns the streams until they close, ctx is cancelled, or Close is
// called. Multiple connections share ownership checks and persistent sessions.
func (c *Client) ServeACP(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("harness is closed")
	}
	a := agent.New(c.service, in, out)
	c.agents[a] = struct{}{}
	c.operations.Add(1)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.agents, a); c.mu.Unlock(); c.operations.Done() }()
	return a.Serve(ctx)
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return c.closeErr
	}
	c.closed = true
	agents := make([]*agent.Agent, 0, len(c.agents))
	for a := range c.agents {
		agents = append(agents, a)
	}
	c.mu.Unlock()
	for _, a := range agents {
		_ = a.Close()
	}
	disconnectErr := c.service.Disconnect(context.Background(), c.owner)
	c.operations.Wait()
	c.closeErr = errors.Join(disconnectErr, c.mcp.Close(), c.store.Close(), c.lock.Close())
	close(c.closeDone)
	return c.closeErr
}
