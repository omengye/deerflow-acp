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

	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/agent"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
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
	// Engine allows embedding a custom execution backend without importing Eino.
	// When nil, the real Eino DeepAgent and durable SQLite stores are used.
	Engine harness.Engine
}

type Client struct {
	service    *hr.Service
	store      *sqlite.Store
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
	engine := cfg.Engine
	if engine == nil {
		engine, err = einoengine.New(ctx, einoengine.Config{Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model, Instruction: cfg.Instruction, MaxIterations: cfg.MaxIterations, CheckpointStore: store, SessionStore: store, ToolFactory: tools.WorkspaceFactory})
		if err != nil {
			return nil, err
		}
	}
	return &Client{service: hr.NewService(business, engine, cfg.Model), store: store, lock: lock, owner: hr.NewID(), agents: make(map[*agent.Agent]struct{}), closeDone: make(chan struct{})}, nil
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
func (c *Client) NewSession(ctx context.Context, cwd string) (harness.Session, error) {
	done, err := c.operation()
	if err != nil {
		return harness.Session{}, err
	}
	defer done()
	return c.service.NewSession(ctx, c.owner, cwd)
}
func (c *Client) LoadSession(ctx context.Context, id, cwd string, replay bool, emit harness.EventHandler) (harness.Session, error) {
	done, err := c.operation()
	if err != nil {
		return harness.Session{}, err
	}
	defer done()
	return c.service.Load(ctx, c.owner, id, cwd, replay, emit)
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
	return c.service.Coordinator.Detach(ctx, id, c.owner)
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
	c.closeErr = errors.Join(disconnectErr, c.store.Close(), c.lock.Close())
	close(c.closeDone)
	return c.closeErr
}
