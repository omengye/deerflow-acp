// Package deerflow assembles the embeddable harness and local ACP service.
package deerflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/agent"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	budgetledger "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
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
	// BackgroundWorkers bounds concurrent native background attempts. Zero uses
	// four workers. Native Eino sessions expose delegation when subagents are on.
	BackgroundWorkers int
	// Nil uses DefaultBudgetLimits. A non-nil zero value disables all quotas.
	// Token accounting is estimated until the provider reports actual usage.
	Budget  *harness.BudgetLimits
	MCP     harness.MCPPolicy
	Sandbox harness.SandboxConfig
	Skills  harness.SkillsConfig
	Media   harness.MediaConfig
	// SkillSelection is host policy. Workspace is derived from each session;
	// global skills require IncludeGlobal. Sources are never installed implicitly.
	SkillSelection harness.SkillSelection
	// MemoryUserID enables workspace-bound user memory for an explicitly known
	// host identity. Empty leaves only session/workspace memory available.
	MemoryUserID string
	// MemoryExtraction enables a bounded post-turn model call that proposes
	// descriptive facts for terminal promotion. Disabled by default.
	MemoryExtraction bool
	// Compaction enables budgeted native Eino session summarization.
	Compaction harness.CompactionConfig
	// Engine allows embedding a custom execution backend without importing Eino.
	// When nil, the real Eino DeepAgent and durable SQLite stores are used.
	Engine harness.Engine
}

type Client struct {
	service    *hr.Service
	store      *sqlite.Store
	mcp        *mcp.Manager
	skills     *skills.Registry
	assets     *assets.Store
	budgets    *budgetledger.Ledger
	background *backgroundHost
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
	if cfg.BackgroundWorkers < 0 || cfg.BackgroundWorkers > 64 {
		return nil, fmt.Errorf("%w: background worker limit must be 0..64", harness.ErrInvalidInput)
	}
	if cfg.MemoryUserID != "" && (len(cfg.MemoryUserID) > 256 || strings.TrimSpace(cfg.MemoryUserID) != cfg.MemoryUserID || strings.ContainsRune(cfg.MemoryUserID, 0) || !utf8.ValidString(cfg.MemoryUserID)) {
		return nil, fmt.Errorf("%w: invalid memory user identity", harness.ErrInvalidInput)
	}
	cfg.Media.VisionModels = slices.Clone(cfg.Media.VisionModels)
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
	limits := harness.DefaultBudgetLimits()
	if cfg.Budget != nil {
		limits = *cfg.Budget
	}
	var background *backgroundHost
	ledger, err := budgetledger.New(store.DB(), budgetledger.Config{CheckEffectTx: func(ctx context.Context, tx *sql.Tx, scope budgetledger.Scope) error {
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT kind FROM budget_members WHERE id=?", scope.MemberID).Scan(&kind); err != nil {
			return err
		}
		if kind == "task" {
			if background == nil {
				return harness.ErrBackgroundUnavailable
			}
			return background.CheckEffectTx(ctx, tx, scope)
		}
		if cfg.Engine == nil {
			return business.CheckExecutionEffectTx(ctx, tx, scope)
		}
		return nil
	}})
	if err != nil {
		return nil, err
	}
	business.BudgetLedger, business.BudgetLimits = ledger, limits
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
	registry, err := skills.NewRegistry(ctx, cfg.Skills, store.DB())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = registry.Close()
		}
	}()
	if len(cfg.Skills.Install) > 128 {
		return nil, fmt.Errorf("too many configured skill installations")
	}
	for _, install := range cfg.Skills.Install {
		record, installErr := registry.Install(ctx, install.SourceID, install.Directory)
		if installErr != nil {
			return nil, fmt.Errorf("install configured skill: %w", installErr)
		}
		if err = registry.SetEnabled(ctx, install.SourceID, record.Ref.Name, install.Enabled); err != nil {
			return nil, fmt.Errorf("configure installed skill: %w", err)
		}
	}
	assetStore, err := assets.NewStore(ctx, filepath.Join(cfg.DataDir, "assets"), store.DB())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = assetStore.Close()
		}
	}()
	memoryStore, err := memory.New(ctx, store.DB())
	if err != nil {
		return nil, err
	}
	engine := cfg.Engine
	if engine == nil {
		baseExtensions := extensionFactory(cfg, manager, registry, assetStore, memoryStore)
		extensions := func(ctx context.Context, req harness.RunRequest, pinned json.RawMessage) (einoengine.RunExtensions, error) {
			out, err := baseExtensions(ctx, req, pinned)
			if err != nil {
				return out, err
			}
			tools, err := background.ToolsForRun(ctx, req, out.State)
			out.Tools = append(out.Tools, tools...)
			return out, err
		}
		engine, err = einoengine.New(ctx, einoengine.Config{Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model, Instruction: cfg.Instruction, MaxIterations: cfg.MaxIterations, Budget: limits, BudgetLedger: ledger, DisableSubAgent: cfg.DisableSubagents, CheckpointStore: store, SessionStore: store, ExtensionFactory: extensions, Media: cfg.Media, AssetResolver: assetStore, ToolImageImporter: assetStore, Compaction: cfg.Compaction})
		if err != nil {
			return nil, err
		}
	}
	service := hr.NewService(business, engine, cfg.Model)
	service.Memory, service.MemoryUserID = memoryStore, cfg.MemoryUserID
	service.Resources = manager
	service.Media, service.Assets = cfg.Media, assetStore
	service.Settings = hr.ConfigSettings{Models: append([]harness.ConfigValue(nil), cfg.Models...), EnableSubagents: !cfg.DisableSubagents, DefaultSubagents: !cfg.DisableSubagents}
	if native, ok := engine.(*einoengine.Engine); ok && cfg.Engine == nil {
		background, err = newBackgroundHost(ctx, cfg, store, service, native, ledger, assetStore, manager, cfg.BackgroundWorkers)
		if err != nil {
			return nil, err
		}
		service.Background = background
		if err = background.service.StartWorkers(context.Background()); err != nil {
			return nil, err
		}
	}
	return &Client{service: service, store: store, mcp: manager, skills: registry, assets: assetStore, budgets: ledger, background: background, lock: lock, owner: hr.NewID(), agents: make(map[*agent.Agent]struct{}), closeDone: make(chan struct{})}, nil
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

func (c *Client) ListToolReceipts(ctx context.Context, id string) ([]harness.ToolReceipt, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.service.ListToolReceipts(ctx, c.owner, id)
}

// ReconcileToolReceipt records the operator's observed outcome. It never
// invokes the tool. An uncertain effect must be reviewed before another run.
func (c *Client) ReconcileToolReceipt(ctx context.Context, id string, review harness.ToolReconciliation) (harness.ToolReceipt, error) {
	done, err := c.operation()
	if err != nil {
		return harness.ToolReceipt{}, err
	}
	defer done()
	return c.service.ReconcileToolReceipt(ctx, c.owner, id, review)
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
	if c.background != nil {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			drainErr := c.background.service.DrainAndClose(ctx)
			timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
			cancel()
			// Close has no deadline. Keep actual worker resources alive until
			// cleanup joins; native close requires bounded waits per attempt.
			if timedOut && !errors.Is(drainErr, harness.ErrBackgroundUncertain) {
				continue
			}
			if drainErr != nil {
				c.closeErr = errors.Join(disconnectErr, drainErr)
				close(c.closeDone)
				return c.closeErr
			}
			break
		}
	}
	c.closeErr = errors.Join(disconnectErr, c.mcp.Close(), c.skills.Close(), c.assets.Close(), c.store.Close(), c.lock.Close())
	close(c.closeDone)
	return c.closeErr
}
