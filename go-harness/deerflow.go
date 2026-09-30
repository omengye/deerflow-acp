// Package deerflow assembles the embeddable harness and local ACP service.
package deerflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	acpclient "github.com/omengye/deerflow-acp/go-harness/internal/acp/client"
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
	// MaxActiveRuns bounds concurrent foreground executions across sessions.
	// Zero uses two slots. QueueTimeout is separate from the execution timeout;
	// zero disables the queue deadline.
	MaxActiveRuns int
	QueueTimeout  time.Duration
	// ContextWindow applies to Model. ContextWindows can specify sizes for
	// additional selectable models; unknown sizes do not produce ACP occupancy.
	ContextWindow  int
	ContextWindows map[string]int
	// Models are the public session choices. ModelRoutes maps non-default choice
	// IDs to host-owned backends; credentials never become session configuration.
	Models           []harness.ConfigValue
	ModelRoutes      map[string]harness.ModelRoute
	DisableSubagents bool
	// ToolPolicy bounds the tool surface for every run, including native Eino
	// task and write_todos. Session configuration cannot enlarge this boundary.
	ToolPolicy harness.ToolPolicy
	// PermissionMode follows local_acp.permission_mode when set. Empty keeps
	// the original Go SDK behavior for existing embedders.
	PermissionMode harness.PermissionMode
	// BackgroundWorkers bounds concurrent native background attempts. Zero uses
	// four workers. Native Eino sessions expose delegation when subagents are on.
	BackgroundWorkers int
	// Nil uses DefaultBudgetLimits. A non-nil zero value disables all quotas.
	// Token accounting is estimated until the provider reports actual usage.
	Budget *harness.BudgetLimits
	MCP    harness.MCPPolicy
	// ACPAgents is an explicit allowlist for optional external stdio delegation.
	// Empty disables the invoke_acp_agent tool.
	ACPAgents map[string]harness.ACPAgentConfig
	Sandbox   harness.SandboxConfig
	Skills    harness.SkillsConfig
	Media     harness.MediaConfig
	// SkillSelection is host policy. Workspace is derived from each session;
	// global skills require IncludeGlobal. Sources are never installed implicitly.
	SkillSelection harness.SkillSelection
	// MemoryUserID enables workspace-bound user memory for an explicitly known
	// host identity. Empty leaves only session/workspace memory available.
	MemoryUserID string
	// MemoryEnabled controls model access to stored facts. Nil preserves the
	// SDK default (enabled). False keeps facts available to operator management
	// while disabling prompt injection, search_memory and extraction.
	MemoryEnabled *bool
	// MemoryScope limits model-visible memory to the selected desktop ACP
	// attachment. Empty preserves the Go SDK's session/workspace/user behavior.
	MemoryScope harness.MemoryScope
	// MemoryMode is empty for the Go SDK's original injection-plus-tool surface.
	// Desktop middleware mode injects facts; tool mode exposes search_memory.
	MemoryMode string
	// Nil retains the SDK defaults. Injection and retrieval are independent:
	// disabling retrieval injects a bounded list of facts in middleware mode.
	MemoryInjectionEnabled *bool
	MemoryRetrievalEnabled *bool
	// MemoryExtraction enables a bounded post-turn model call that proposes
	// descriptive facts for terminal promotion. Disabled by default.
	MemoryExtraction bool
	// Compaction enables budgeted native Eino session summarization.
	Compaction harness.CompactionConfig
	// Retention controls optional automatic cleanup of detached sessions.
	Retention harness.RetentionPolicy
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
	retention  harness.RetentionPolicy
	lock       *flock.Flock
	owner      string
	mu         sync.Mutex
	manageMu   sync.Mutex
	closed     bool
	agents     map[*agent.Agent]struct{}
	operations sync.WaitGroup
	closeDone  chan struct{}
	closeErr   error
}

func Open(ctx context.Context, cfg Config) (client *Client, err error) {
	if cfg.MaxActiveRuns == 0 {
		cfg.MaxActiveRuns = 2
	}
	if cfg.MaxActiveRuns < 1 || cfg.MaxActiveRuns > 128 || cfg.QueueTimeout < 0 || cfg.QueueTimeout > 24*time.Hour {
		return nil, fmt.Errorf("%w: invalid run queue limits", harness.ErrInvalidInput)
	}
	if err := cfg.PermissionMode.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
	}
	if err := cfg.ToolPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
	}
	if !cfg.ToolPolicy.Allows("task") {
		cfg.DisableSubagents = true
	}
	if cfg.ToolPolicy.Allowlist != nil {
		cfg.ToolPolicy.Allowlist = append([]string{}, cfg.ToolPolicy.Allowlist...)
	}
	if cfg.ToolPolicy.Denylist != nil {
		cfg.ToolPolicy.Denylist = append([]string{}, cfg.ToolPolicy.Denylist...)
	}
	if err := hr.ValidateRetention(cfg.Retention); err != nil {
		return nil, fmt.Errorf("%w: invalid session retention policy", err)
	}
	if cfg.ContextWindow < 0 || cfg.ContextWindow > 1<<30 {
		return nil, fmt.Errorf("%w: context window must be 0..2^30", harness.ErrInvalidInput)
	}
	contextWindows := make(map[string]int, len(cfg.ContextWindows)+1)
	for modelID, size := range cfg.ContextWindows {
		if modelID == "" || size <= 0 || size > 1<<30 {
			return nil, fmt.Errorf("%w: invalid context window for model %q", harness.ErrInvalidInput, modelID)
		}
		contextWindows[modelID] = size
	}
	if cfg.ContextWindow > 0 {
		if cfg.Model == "" {
			return nil, fmt.Errorf("%w: context window requires a default model", harness.ErrInvalidInput)
		}
		contextWindows[cfg.Model] = cfg.ContextWindow
	}
	if cfg.BackgroundWorkers < 0 || cfg.BackgroundWorkers > 64 {
		return nil, fmt.Errorf("%w: background worker limit must be 0..64", harness.ErrInvalidInput)
	}
	if cfg.MemoryUserID != "" && (len(cfg.MemoryUserID) > 256 || strings.TrimSpace(cfg.MemoryUserID) != cfg.MemoryUserID || strings.ContainsRune(cfg.MemoryUserID, 0) || !utf8.ValidString(cfg.MemoryUserID)) {
		return nil, fmt.Errorf("%w: invalid memory user identity", harness.ErrInvalidInput)
	}
	if cfg.MemoryScope != "" && cfg.MemoryScope != harness.MemorySession && cfg.MemoryScope != harness.MemoryWorkspace {
		return nil, fmt.Errorf("%w: memory scope must be session or workspace; global is not supported by the Go store", harness.ErrInvalidInput)
	}
	if cfg.MemoryMode != "" && cfg.MemoryMode != "middleware" && cfg.MemoryMode != "tool" {
		return nil, fmt.Errorf("%w: memory mode must be middleware or tool", harness.ErrInvalidInput)
	}
	if cfg.MemoryMode == "tool" && cfg.MemoryRetrievalEnabled != nil && !*cfg.MemoryRetrievalEnabled && (cfg.MemoryEnabled == nil || *cfg.MemoryEnabled) {
		return nil, fmt.Errorf("%w: memory tool mode requires retrieval_enabled=true", harness.ErrInvalidInput)
	}
	cfg.Media.VisionModels = slices.Clone(cfg.Media.VisionModels)
	cfg.ModelRoutes = maps.Clone(cfg.ModelRoutes)
	cfg.ACPAgents = maps.Clone(cfg.ACPAgents)
	for name, agent := range cfg.ACPAgents {
		agent.Args = slices.Clone(agent.Args)
		agent.Env = maps.Clone(agent.Env)
		cfg.ACPAgents[name] = agent
	}
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
	if len(cfg.ACPAgents) > 0 {
		if _, err = acpclient.Tool(cfg.DataDir, harness.Session{ID: "configuration"}, cfg.ACPAgents); err != nil {
			return nil, err
		}
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
	if err = acpclient.CleanupOrphans(ctx, cfg.DataDir, store.DB()); err != nil {
		return nil, fmt.Errorf("cleanup external ACP session orphans: %w", err)
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
			return nil, fmt.Errorf("install configured skill %s/%s: %w", install.SourceID, install.Directory, installErr)
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
		modelMemory := memoryStore
		if cfg.MemoryEnabled != nil && !*cfg.MemoryEnabled {
			modelMemory = nil
			cfg.MemoryExtraction = false
		}
		baseExtensions := extensionFactory(cfg, manager, registry, assetStore, modelMemory)
		extensions := func(ctx context.Context, req harness.RunRequest, pinned json.RawMessage) (einoengine.RunExtensions, error) {
			out, err := baseExtensions(ctx, req, pinned)
			if err != nil {
				return out, err
			}
			tools, err := background.ToolsForRun(ctx, req, out.State)
			out.Tools = append(out.Tools, tools...)
			return out, err
		}
		engine, err = einoengine.New(ctx, einoengine.Config{Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model, ModelRoutes: cfg.ModelRoutes, Instruction: cfg.Instruction, MaxIterations: cfg.MaxIterations, Budget: limits, BudgetLedger: ledger, DisableSubAgent: cfg.DisableSubagents, ToolPolicy: cfg.ToolPolicy, PermissionMode: cfg.PermissionMode, CheckpointStore: store, SessionStore: store, ExtensionFactory: extensions, Media: cfg.Media, AssetResolver: assetStore, ToolImageImporter: assetStore, ModelImageImporter: assetStore, ToolOutputStore: assetStore, Compaction: cfg.Compaction, ContextWindows: contextWindows})
		if err != nil {
			return nil, err
		}
	}
	service, err := hr.NewServiceWithRunQueue(business, engine, cfg.Model, hr.RunQueueConfig{MaxActiveRuns: cfg.MaxActiveRuns, QueueTimeout: cfg.QueueTimeout})
	if err != nil {
		return nil, err
	}
	service.PermissionMode = cfg.PermissionMode
	if len(cfg.ACPAgents) > 0 {
		service.ExternalPromptPending = func(ctx context.Context, owner, sessionID, agent string) (harness.ExternalPromptState, error) {
			return pendingExternalPrompt(ctx, service, cfg.DataDir, cfg.ACPAgents, owner, sessionID, agent)
		}
		service.ExternalPromptAcknowledge = func(ctx context.Context, owner, sessionID, agent, promptID string) error {
			return acknowledgeExternalPrompt(ctx, service, cfg.DataDir, cfg.ACPAgents, owner, sessionID, agent, promptID)
		}
	}
	if len(cfg.ACPAgents) > 0 {
		service.SessionCleanup = func(id string) error { return acpclient.CleanupSession(cfg.DataDir, id) }
	}
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
	return &Client{service: service, store: store, mcp: manager, skills: registry, assets: assetStore, budgets: ledger, background: background, retention: cfg.Retention, lock: lock, owner: hr.NewID(), agents: make(map[*agent.Agent]struct{}), closeDone: make(chan struct{})}, nil
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
