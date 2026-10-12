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

	"github.com/cloudwego/eino/components/tool"
	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/agent"
	acpclient "github.com/omengye/deerflow-acp/go-harness/internal/acp/client"
	agentregistry "github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	budgetledger "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type Config struct {
	DataDir     string
	Provider    string
	APIKey      string
	BaseURL     string
	Model       string
	Instruction string
	// OutputTokenParameter controls only the default route's wire field.
	// Budget.MaxOutputTokens remains the single per-request output budget.
	OutputTokenParameter harness.OutputTokenParameter
	// MaxIterations is an optional per-agent loop limit; zero disables it.
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
	Models      []harness.ConfigValue
	ModelRoutes map[string]harness.ModelRoute
	// ModelReasoningEfforts allowlists the effort levels supported by each
	// configured model. Missing/empty entries do not expose an ACP selector.
	ModelReasoningEfforts map[string][]string
	DisableSubagents      bool
	// Subagents defines host-owned builtin overrides and custom roles. Runtime
	// model references are resolved by the configuration loader before Open.
	Subagents *harness.SubagentsConfig
	// ToolPolicy bounds the tool surface for every run, including native Eino
	// task and write_todos. Session configuration cannot enlarge this boundary.
	ToolPolicy harness.ToolPolicy
	// BuiltinTools maps the portable tools list to native Go implementations.
	// HostToolsAllowed separately opts into host_opencli. Nil preserves the SDK
	// workspace tool surface and does not enable any network or host adapter.
	BuiltinTools     []harness.BuiltinToolConfig
	HostToolsAllowed bool
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
	// HostMCPRecipes grants separate, exact-workspace host resource authority.
	// Nil preserves client-owned MCP behavior and never reconstructs client data.
	HostMCPRecipes []harness.HostMCPRecipe
	// ACPAgents is an explicit allowlist for optional external stdio delegation.
	// Empty disables the invoke_acp_agent tool.
	ACPAgents map[string]harness.ACPAgentConfig
	Sandbox   harness.SandboxConfig
	Skills    harness.SkillsConfig
	// SkillValidator is a trusted, resource-bounded evaluator for operator
	// proposals. Nil keeps history/review readable but new candidates fail
	// validation. This dependency alone does not enable model skill tools.
	SkillValidator harness.SkillValidator
	// SkillGeneration enables an independently budgeted worker for explicit
	// operator requests. Nil leaves generation disabled and history readable.
	SkillGeneration *harness.SkillGenerationOptions
	// SkillLearning opts into delayed procedural extraction and, separately,
	// bounded consolidation and authorized skill matching. It
	// neither enables discovery nor publishes skills. Its explicit sublimits
	// stay inside the same maintenance budget when generation is configured.
	SkillLearning               *harness.SkillLearningOptions
	SkillExperienceExtractor    harness.SkillExperienceExtractor
	SkillExperienceConsolidator harness.SkillExperienceConsolidator
	SkillExperienceMatcher      harness.SkillExperienceMatcher
	// SkillDiscovery explicitly opts into selected automatic proposal triggers.
	// It requires SkillGeneration and shares its maintenance budget; experience
	// patterns additionally require all learning stages; publication needs a
	// separate learning auto_patch grant or explicit review.
	// Nil leaves automatic selection disabled, including after migration.
	SkillDiscovery *harness.SkillDiscoveryOptions
	// SkillAutoPatch enables bounded publication and durable regression monitoring.
	// ExperiencePatternsEnabled is an independent learning publication opt-in.
	SkillAutoPatch *harness.SkillAutoPatchOptions
	// SkillBehavior evaluates host-defined fixtures using the independent
	// maintenance worker. Both options and evaluator must be supplied together.
	// It requires SkillGeneration; nil keeps behavioral execution disabled.
	SkillBehavior          *harness.SkillBehaviorOptions
	SkillBehaviorEvaluator harness.SkillBehaviorEvaluator
	// SkillProposalSources explicitly grants model candidate submission to
	// these configured sources, still intersected with the run's workspace,
	// visible skill snapshot and role policy. Empty disables skill_manage.
	// Model tools never receive approval or rollback authority.
	SkillProposalSources []string
	skillEvolution       *evolution.Service
	Media                harness.MediaConfig
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
	// MemoryScope selects the local memory range. Global selects workspace
	// plus global; empty preserves the SDK's session/workspace/user ranges.
	MemoryScope harness.MemoryScope
	// MemoryIncludeGlobal adds this portable profile's global facts to the
	// selected local scope. Nil enables sharing; extraction stays local.
	MemoryIncludeGlobal *bool
	memoryGlobalID      string
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
	evolution  *evolution.Service
	learning   *learning.Store
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
	if cfg.MaxActiveRuns < 1 || cfg.MaxActiveRuns > 128 || cfg.QueueTimeout < 0 || cfg.QueueTimeout > harness.MaxExecutionTimeoutSeconds*time.Second {
		return nil, fmt.Errorf("%w: invalid run queue limits", harness.ErrInvalidInput)
	}
	if err := cfg.PermissionMode.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
	}
	if err := cfg.ToolPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
	}
	if cfg.Subagents != nil {
		models := map[string]bool{cfg.Model: true}
		for _, choice := range cfg.Models {
			models[choice.Value] = true
		}
		for name := range cfg.ModelRoutes {
			models[name] = true
		}
		if _, err := agentregistry.New(*cfg.Subagents, models); err != nil {
			return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
		}
		if cfg.Subagents.Enabled != nil && !*cfg.Subagents.Enabled {
			cfg.DisableSubagents = true
		}
	}
	cfg.BuiltinTools = slices.Clone(cfg.BuiltinTools)
	toolNames := map[string]bool{}
	for i, config := range cfg.BuiltinTools {
		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
		}
		if toolNames[config.Name] {
			return nil, fmt.Errorf("%w: duplicate configured tool %s", harness.ErrInvalidInput, config.Name)
		}
		toolNames[config.Name] = true
		cfg.BuiltinTools[i].AllowedSites = slices.Clone(config.AllowedSites)
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
	if cfg.MemoryScope != "" && cfg.MemoryScope != harness.MemorySession && cfg.MemoryScope != harness.MemoryWorkspace && cfg.MemoryScope != harness.MemoryGlobal {
		return nil, fmt.Errorf("%w: memory scope must be session, workspace, or global", harness.ErrInvalidInput)
	}
	if cfg.MemoryScope == harness.MemoryGlobal && cfg.MemoryIncludeGlobal != nil && !*cfg.MemoryIncludeGlobal && (cfg.MemoryEnabled == nil || *cfg.MemoryEnabled) {
		return nil, fmt.Errorf("%w: global memory scope requires include_global=true", harness.ErrInvalidInput)
	}
	if cfg.MemoryMode != "" && cfg.MemoryMode != "middleware" && cfg.MemoryMode != "tool" {
		return nil, fmt.Errorf("%w: memory mode must be middleware or tool", harness.ErrInvalidInput)
	}
	if cfg.MemoryMode == "tool" && cfg.MemoryRetrievalEnabled != nil && !*cfg.MemoryRetrievalEnabled && (cfg.MemoryEnabled == nil || *cfg.MemoryEnabled) {
		return nil, fmt.Errorf("%w: memory tool mode requires retrieval_enabled=true", harness.ErrInvalidInput)
	}
	cfg.Media.VisionModels = slices.Clone(cfg.Media.VisionModels)
	cfg.ModelRoutes = maps.Clone(cfg.ModelRoutes)
	cfg.ModelReasoningEfforts = maps.Clone(cfg.ModelReasoningEfforts)
	for name, efforts := range cfg.ModelReasoningEfforts {
		provider := cfg.Provider
		if route, ok := cfg.ModelRoutes[name]; ok {
			provider = route.Provider
		}
		if err := harness.ValidateReasoningEfforts(provider, efforts); err != nil {
			return nil, fmt.Errorf("model %q: %w", name, err)
		}
		cfg.ModelReasoningEfforts[name] = slices.Clone(efforts)
	}
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
	if cfg.SkillLearning == nil && cfg.SkillExperienceExtractor != nil {
		return nil, fmt.Errorf("%w: extractor requires skill learning opt-in", harness.ErrInvalidInput)
	}
	if cfg.SkillExperienceConsolidator != nil && (cfg.SkillLearning == nil || cfg.SkillLearning.Consolidation == nil) {
		return nil, fmt.Errorf("%w: consolidator requires explicit learning consolidation opt-in", harness.ErrInvalidInput)
	}
	if cfg.SkillExperienceMatcher != nil && (cfg.SkillLearning == nil || cfg.SkillLearning.Matching == nil) {
		return nil, fmt.Errorf("%w: matcher requires explicit learning matching opt-in", harness.ErrInvalidInput)
	}
	if cfg.SkillLearning != nil {
		copy := *cfg.SkillLearning
		if copy.Consolidation != nil {
			model := *copy.Consolidation
			copy.Consolidation = &model
		}
		if copy.Matching != nil {
			model := *copy.Matching
			copy.Matching = &model
		}
		cfg.SkillLearning = &copy
		if cfg.SkillGeneration != nil && cfg.SkillGeneration.Budget != copy.Budget {
			return nil, fmt.Errorf("%w: learning and generation must share one maintenance budget", harness.ErrInvalidInput)
		}
	}
	learningStore, err := learning.New(ctx, store.DB(), cfg.SkillLearning)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = learningStore.Close()
		}
	}()
	business.Learning = learningStore
	if err = learningStore.ConfigureRootEvidence(business.LearningRootObservationsTx); err != nil {
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
	ledger, err := budgetledger.New(store.DB(), budgetledger.Config{UsageOwnerTx: einoengine.BudgetUsageOwnerTx, CheckEffectTx: func(ctx context.Context, tx *sql.Tx, scope budgetledger.Scope) error {
		if err := einoengine.CheckTaskTreeEffectTx(ctx, tx, scope.RootBudgetID); err != nil {
			return err
		}
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT kind FROM budget_members WHERE id=?", scope.MemberID).Scan(&kind); err != nil {
			return err
		}
		if kind == "task" || kind == "background_continuation" {
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
	if err = manager.ConfigureHostRecipes(cfg.HostMCPRecipes); err != nil {
		return nil, err
	}
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
		if err = registry.InstallConfigured(ctx, install.SourceID, install.Directory, install.Enabled); err != nil {
			return nil, fmt.Errorf("install configured skill %s/%s: %w", install.SourceID, install.Directory, err)
		}
	}
	if err = registry.ObserveImports(ctx); err != nil {
		return nil, fmt.Errorf("observe managed skill imports: %w", err)
	}
	if err = registry.ReconcileExports(ctx, false); err != nil {
		return nil, fmt.Errorf("restore managed skill exports: %w", err)
	}
	skillEvolution, err := evolution.New(ctx, registry, cfg.SkillValidator)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = skillEvolution.Close()
		}
	}()
	if cfg.SkillGeneration != nil {
		generator, buildErr := einoengine.NewSkillCandidateGenerator(ctx, cfg.SkillGeneration.Model)
		if buildErr != nil {
			return nil, buildErr
		}
		if err = skillEvolution.ConfigureGeneration(ctx, generator, cfg.SkillGeneration.Budget, cfg.SkillGeneration.Sources, cfg.SkillGeneration.AllowBuiltinOverlays); err != nil {
			return nil, err
		}
	}
	if cfg.SkillLearning != nil {
		extractor := cfg.SkillExperienceExtractor
		if extractor == nil {
			extractor, err = einoengine.NewSkillExperienceExtractor(ctx, cfg.SkillLearning.Model)
			if err != nil {
				return nil, err
			}
		}
		queue := skillEvolution.MaintenanceQueue()
		if err = learningStore.Configure(ctx, extractor, queue); err != nil {
			return nil, err
		}
		if cfg.SkillLearning.Consolidation != nil {
			consolidator := cfg.SkillExperienceConsolidator
			if consolidator == nil {
				consolidator, err = einoengine.NewSkillExperienceConsolidator(ctx, *cfg.SkillLearning.Consolidation)
				if err != nil {
					return nil, err
				}
			}
			if err = learningStore.ConfigureConsolidation(consolidator); err != nil {
				return nil, err
			}
		}
		if cfg.SkillLearning.Matching != nil {
			selection := cfg.SkillSelection
			if !cfg.ToolPolicy.Allows("skill") && !cfg.ToolPolicy.Allows("read_skill_file") {
				selection.Names = []string{}
			}
			access, buildErr := registry.NewMatchAccess(selection)
			if buildErr != nil {
				return nil, buildErr
			}
			matcher := cfg.SkillExperienceMatcher
			if matcher == nil {
				matcher, err = einoengine.NewSkillExperienceMatcher(ctx, *cfg.SkillLearning.Matching)
				if err != nil {
					return nil, err
				}
			}
			if err = learningStore.ConfigureMatching(matcher, access); err != nil {
				return nil, err
			}
		}
		if queue != nil {
			err = skillEvolution.RegisterMaintenanceProducer(skillmaintenance.ExperienceExtract, learningStore.ExtractionHooks(), learningStore.Handle)
			if err != nil {
				return nil, err
			}
			if cfg.SkillLearning.Consolidation != nil {
				if err = skillEvolution.RegisterMaintenanceProducer(skillmaintenance.ExperienceConsolidate, learningStore.ConsolidationHooks(), learningStore.HandleConsolidation); err != nil {
					return nil, err
				}
			}
			if cfg.SkillLearning.Matching != nil {
				if err = skillEvolution.RegisterMaintenanceProducer(skillmaintenance.SkillMatch, learningStore.MatchingHooks(), learningStore.HandleMatch); err != nil {
					return nil, err
				}
			}
		}
	}
	if (cfg.SkillBehavior == nil) != (cfg.SkillBehaviorEvaluator == nil) {
		return nil, fmt.Errorf("%w: skill behavior requires both options and evaluator", harness.ErrInvalidInput)
	}
	if cfg.SkillBehavior != nil {
		if err = skillEvolution.ConfigureBehavior(cfg.SkillBehaviorEvaluator, *cfg.SkillBehavior); err != nil {
			return nil, err
		}
	}
	if cfg.SkillGeneration != nil && cfg.SkillLearning != nil && cfg.SkillLearning.Matching != nil {
		if err = skillEvolution.ConfigureExperienceDiscoverySource(learningStore); err != nil {
			return nil, err
		}
	}
	if cfg.SkillDiscovery != nil {
		if err = skillEvolution.ConfigureDiscovery(*cfg.SkillDiscovery); err != nil {
			return nil, err
		}
	}
	if cfg.SkillAutoPatch != nil {
		if err = skillEvolution.ConfigureAutoPatch(*cfg.SkillAutoPatch); err != nil {
			return nil, err
		}
	}
	cfg.SkillProposalSources = slices.Clone(cfg.SkillProposalSources)
	if err = validateSkillProposalSources(cfg, registry); err != nil {
		return nil, err
	}
	cfg.skillEvolution = skillEvolution
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
	cfg.memoryGlobalID, err = memoryStore.ProfileID(ctx)
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
			state := append(json.RawMessage(nil), out.State...)
			out.RoleToolFactory = func(ctx context.Context) ([]tool.BaseTool, error) {
				return background.RoleToolsForRun(ctx, req, state)
			}
			return out, err
		}
		delegations, admissionErr := agentregistry.NewController(store.DB())
		if admissionErr != nil {
			return nil, admissionErr
		}
		engine, err = einoengine.New(ctx, einoengine.Config{Delegations: delegations, Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model, OutputTokenParameter: cfg.OutputTokenParameter, ModelRoutes: cfg.ModelRoutes, ModelReasoningEfforts: cfg.ModelReasoningEfforts, Models: cfg.Models, Instruction: cfg.Instruction, MaxIterations: cfg.MaxIterations, Budget: limits, BudgetLedger: ledger, DisableSubAgent: cfg.DisableSubagents, Subagents: cfg.Subagents, ToolPolicy: cfg.ToolPolicy, PermissionMode: cfg.PermissionMode, CheckpointStore: store, SessionStore: store, ExtensionFactory: extensions, Media: cfg.Media, AssetResolver: assetStore, ToolImageImporter: assetStore, ModelImageImporter: assetStore, ToolOutputStore: assetStore, TaskContextStore: business, Compaction: cfg.Compaction, ContextWindows: contextWindows})
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
	service.MemoryGlobalID = cfg.memoryGlobalID
	service.Resources = manager
	service.Media, service.Assets = cfg.Media, assetStore
	service.Settings = hr.ConfigSettings{Models: append([]harness.ConfigValue(nil), cfg.Models...), ModelReasoningEfforts: cfg.ModelReasoningEfforts, EnableSubagents: !cfg.DisableSubagents, DefaultSubagents: !cfg.DisableSubagents}
	if native, ok := engine.(*einoengine.Engine); ok && cfg.Engine == nil {
		background, err = newBackgroundHost(ctx, cfg, store, service, native, ledger, assetStore, manager, cfg.BackgroundWorkers)
		if err != nil {
			return nil, err
		}
		service.Background = background
		if err = background.restoreHostResources(ctx); err != nil {
			return nil, err
		}
		if err = background.service.StartWorkers(context.Background()); err != nil {
			return nil, err
		}
	}
	if err = registry.StartExportWorker(); err != nil {
		return nil, err
	}
	if err = skillEvolution.StartCollector(); err != nil {
		return nil, err
	}
	if err = skillEvolution.StartGeneration(); err != nil {
		return nil, err
	}
	if err = skillEvolution.StartDiscovery(); err != nil {
		return nil, err
	}
	if err = skillEvolution.StartAutoPatch(); err != nil {
		return nil, err
	}
	if err = learningStore.Start(); err != nil {
		return nil, err
	}
	return &Client{service: service, store: store, mcp: manager, skills: registry, evolution: skillEvolution, learning: learningStore, assets: assetStore, budgets: ledger, background: background, retention: cfg.Retention, lock: lock, owner: hr.NewID(), agents: make(map[*agent.Agent]struct{}), closeDone: make(chan struct{})}, nil
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
func (c *Client) Cancel(id string) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.service.Cancel(ctx, c.owner, id)
}

// ServeSessionUpdates resumes ready durable waits and processes terminal task
// notifications whose saved automatic-continuation policy allows new runs.
// It runs until ctx ends, the session closes, or this client closes. Run it in
// a caller-owned goroutine. Handlers must honor cancellation and must not
// synchronously close the client/session from inside a callback.
func (c *Client) ServeSessionUpdates(ctx context.Context, id string, handlers harness.SessionUpdates) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.service.ServeSessionUpdates(ctx, c.owner, id, handlers)
}
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
	// Stop and join local evidence collection before any dependency is closed,
	// including the error path where background resource draining fails.
	collectorErr := errors.Join(c.evolution.Close(), c.learning.Close())
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
				c.closeErr = errors.Join(disconnectErr, collectorErr, drainErr)
				close(c.closeDone)
				return c.closeErr
			}
			break
		}
	}
	c.closeErr = errors.Join(disconnectErr, collectorErr, c.mcp.Close(), c.skills.Close(), c.assets.Close(), c.store.Close(), c.lock.Close())
	close(c.closeDone)
	return c.closeErr
}
