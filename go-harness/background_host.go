package deerflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

const backgroundAgentVersion = "deerflow-child-v1"
const backgroundRoleVersion = "deerflow-role-v2"
const backgroundCatalogVersion = "deerflow-role-v3"
const backgroundTreeVersion = "deerflow-tree-v4"
const backgroundMixedVersion = "deerflow-mixed-v5"

// backgroundHost rebuilds every native attempt from durable child-owned state.
// It is process-owned; no foreground tool/provider closure is registered with
// the native task manager. Public assembly enables it only with the full broker.
type backgroundHost struct {
	store             *sqlite.Store
	runtime           *hr.Service
	engine            *einoengine.Engine
	assets            *assets.Store
	mcp               *mcp.Manager
	specs             *hr.BackgroundExecutionStore
	interactions      *hr.BackgroundInteractionStore
	ledger            *background.LedgerAdapter
	service           *background.Service
	policy            string
	legacyPolicy      string
	roleV2Policy      string
	resourceMu        sync.Mutex
	resourceAdmission sync.RWMutex
	resourceAttempts  map[string]*backgroundResourceAttempt
}

type backgroundSpecKey struct{}

func backgroundHostPolicy(cfg Config) (string, error) {
	return backgroundHostPolicyVersion(cfg, 1)
}

func backgroundHostPolicyVersion(cfg Config, version int) (string, error) {
	// Provider credentials stay process-local. Their presence is neither an
	// execution grant nor part of an immutable checkpoint resource identity.
	var toolPolicy *harness.ToolPolicy
	if cfg.ToolPolicy.Allowlist != nil || len(cfg.ToolPolicy.Denylist) > 0 {
		toolPolicy = &cfg.ToolPolicy
	}
	var routes map[string]harness.ModelRoute
	if (version >= 3 || cfg.Subagents != nil) && cfg.ModelRoutes != nil {
		routes = make(map[string]harness.ModelRoute, len(cfg.ModelRoutes))
		for name, route := range cfg.ModelRoutes {
			route.APIKey = ""
			routes[name] = route
		}
	}
	if version >= 3 {
		cfg.Subagents = nil
	}
	data, err := json.Marshal(struct {
		Version                               int
		Provider, BaseURL, Model, Instruction string
		MaxIterations                         int
		DisableSubagents                      bool
		Media                                 harness.MediaConfig
		ToolPolicy                            *harness.ToolPolicy `json:",omitempty"`
		PermissionMode                        harness.PermissionMode
		Subagents                             *harness.SubagentsConfig      `json:",omitempty"`
		ModelRoutes                           map[string]harness.ModelRoute `json:",omitempty"`
	}{version, cfg.Provider, cfg.BaseURL, cfg.Model, cfg.Instruction, cfg.MaxIterations, cfg.DisableSubagents, cfg.Media, toolPolicy, cfg.PermissionMode, cfg.Subagents, routes})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func newBackgroundHost(ctx context.Context, cfg Config, store *sqlite.Store, runtime *hr.Service, engine *einoengine.Engine, ledger *budget.Ledger, assets *assets.Store, manager *mcp.Manager, workers int) (*backgroundHost, error) {
	if store == nil || runtime == nil || engine == nil || ledger == nil {
		return nil, harness.ErrBackgroundUnavailable
	}
	h := &backgroundHost{store: store, runtime: runtime, engine: engine, assets: assets, mcp: manager}
	var err error
	if h.policy, err = backgroundHostPolicyVersion(cfg, 3); err != nil {
		return nil, err
	}
	if h.roleV2Policy, err = backgroundHostPolicy(cfg); err != nil {
		return nil, err
	}
	legacy := cfg
	legacy.Subagents = nil
	if h.legacyPolicy, err = backgroundHostPolicy(legacy); err != nil {
		return nil, err
	}
	runtime.ContinuationHostPolicy = h.policy
	runtime.ContinuationHostPolicies = map[int]string{1: h.legacyPolicy, 2: h.roleV2Policy, 3: h.policy, 4: h.policy, 5: h.policy, 6: h.policy}
	if h.specs, err = hr.NewBackgroundExecutionStore(ctx, runtime.Store); err != nil {
		return nil, err
	}
	if err = h.initResourceStore(ctx); err != nil {
		return nil, err
	}
	runtime.BeforeResourceRelease = h.releaseResourceSession
	runtime.CheckContinuationResources = h.checkContinuationResources
	runtime.ContinuationResourceGeneration = h.continuationResourceGeneration
	runtime.PrepareContinuationResources = h.prepareContinuationResourceLease
	if h.interactions, err = hr.NewBackgroundInteractionStore(ctx, runtime.Store, h); err != nil {
		return nil, err
	}
	h.interactions.PermissionMode = cfg.PermissionMode
	h.interactions.Assets = assets
	if h.ledger, err = background.NewLedgerAdapter(store, ledger); err != nil {
		return nil, err
	}
	h.service, err = background.New(ctx, background.Config{
		Store: store, Authorizer: h, Budgets: h, Approvals: h.interactions, Waits: h.interactions, Attempts: h,
		AgentNames: []string{backgroundAgentVersion, backgroundRoleVersion, backgroundCatalogVersion, backgroundTreeVersion, backgroundMixedVersion, hr.BackgroundContinuationAgentVersion}, MaxWorkers: workers,
		RolePlacement: func(ctx context.Context, tx *sql.Tx) (*background.RolePlacement, error) {
			role, active, err := einoengine.CurrentRoleTx(ctx, tx)
			if err != nil || !active {
				return nil, err
			}
			return &background.RolePlacement{ID: role.ID, RootBudgetID: role.RootID, Depth: role.Depth}, nil
		},
		HeartbeatInterval: min(5*time.Second, h.ledger.HeartbeatInterval()),
		OnTransitionTx:    h.transitionTx,
	})
	if err != nil {
		return nil, err
	}
	runtime.BackgroundContinuation = h.processContinuation
	return h, nil
}

type backgroundContinuationKey struct{}

func (h *backgroundHost) processContinuation(ctx context.Context, owner, root, notificationID string) (harness.BackgroundTask, error) {
	c, existing, err := h.runtime.PrepareBackgroundContinuation(ctx, owner, root, notificationID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	actor := harness.TaskActor{OwnerID: owner, SessionID: root}
	if existing != "" {
		return h.service.Get(ctx, actor, existing)
	}
	ctx = c.Context(context.WithValue(ctx, backgroundContinuationKey{}, c))
	ctx = context.WithValue(ctx, backgroundSpecKey{}, c.Spec)
	return h.service.SubmitContinuation(ctx, actor, c.Binding, c.Input, c.Description(), c.CheckTx)
}

func (h *backgroundHost) AuthorizeTaskAccess(ctx context.Context, actor harness.TaskActor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.runtime.Coordinator.Authorize(actor.SessionID, actor.OwnerID)
}

func (h *backgroundHost) Validate(ctx context.Context, binding background.Binding) error {
	var spec hr.BackgroundExecutionSpec
	if proposed, ok := ctx.Value(backgroundSpecKey{}).(hr.BackgroundExecutionSpec); ok {
		spec = proposed
	} else {
		var err error
		_, spec, err = h.specs.Load(ctx, binding)
		if err != nil {
			return err
		}
	}
	contract, err := spec.Contract()
	if err != nil {
		return err
	}
	wantVersion, wantPolicy := backgroundAgentVersion, h.legacyPolicy
	if spec.Version == 2 {
		wantVersion, wantPolicy = backgroundRoleVersion, h.roleV2Policy
	} else if spec.Version == 3 {
		wantVersion, wantPolicy = backgroundCatalogVersion, h.policy
	} else if spec.Version == 4 {
		wantVersion, wantPolicy = backgroundTreeVersion, h.policy
	} else if spec.Version == 5 {
		wantVersion, wantPolicy = backgroundMixedVersion, h.policy
	} else if spec.Version == 6 {
		wantVersion, wantPolicy = hr.BackgroundContinuationAgentVersion, h.policy
	}
	if spec.HostPolicy != wantPolicy || binding.AgentVersion != wantVersion || spec.AgentVersion != wantVersion || binding.ExecutionContract != contract {
		return harness.ErrTaskOriginConflict
	}
	var extension extensionState
	if err = json.Unmarshal(spec.Extension, &extension); err != nil || extension.Version != 1 {
		return harness.ErrBackgroundUnavailable
	}
	if extension.MCPGeneration != "" {
		return harness.ErrResourcesIncompatible
	}
	if err = h.validateResources(ctx, binding, spec); err != nil {
		return err
	}
	tx, err := h.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = h.specs.CheckParentPolicyTx(ctx, tx, binding, spec); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *backgroundHost) BindTaskTx(ctx context.Context, tx *sql.Tx, binding background.Binding) error {
	spec, ok := ctx.Value(backgroundSpecKey{}).(hr.BackgroundExecutionSpec)
	if !ok {
		return harness.ErrTaskOriginConflict
	}
	if err := h.specs.BindTx(ctx, tx, binding, spec); err != nil {
		return err
	}
	if err := h.bindResourcesTx(ctx, tx, binding, spec); err != nil {
		return err
	}
	if spec.Continuation != nil {
		c, ok := ctx.Value(backgroundContinuationKey{}).(*hr.BackgroundContinuation)
		if !ok {
			return harness.ErrPermissionDenied
		}
		if err := c.CommitTx(ctx, tx, binding); err != nil {
			return err
		}
		return h.ledger.BindTaskTx(ctx, tx, binding)
	}
	if spec.Role != nil {
		engine := h.engine
		if spec.Roles != nil {
			var err error
			engine, err = engine.WithRoleCatalog(spec.Roles)
			if err != nil {
				return err
			}
		}
		placement := einoengine.RolePlacement{Depth: 1}
		if spec.Version >= 4 {
			placement.Depth = spec.Depth
			if spec.ParentTaskID != "" || spec.ParentInvocationID != "" {
				placement.ParentID = "background/" + spec.ParentTaskID
				if spec.Version == 5 {
					placement.ParentID = spec.ParentInvocationID
				}
				parent, err := h.specs.ParentRoleTx(ctx, tx, binding, spec)
				if err != nil {
					return err
				}
				resolved, err := engine.ResolveDescendant(spec.Role.Name, *parent, spec.Depth-1)
				if err != nil {
					return err
				}
				if resolved.Version != spec.Role.Version {
					return harness.ErrPermissionDenied
				}
			}
		}
		if err := engine.BindPlacedRoleTx(ctx, tx, binding.RootBudgetID, binding.TaskID, *spec.Role, placement); err != nil {
			return err
		}
	}
	return h.ledger.BindTaskTx(ctx, tx, binding)
}

func (h *backgroundHost) BeforeAttempt(ctx context.Context, scope background.TaskScope) error {
	tx, err := h.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = h.specs.BeforeAttemptTx(ctx, tx, scope, h.ledger.BeforeAttemptTx); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *backgroundHost) CommitAttemptTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, status string) error {
	return h.specs.CommitAttemptTx(ctx, tx, scope, status, h.ledger.CommitAttemptTx)
}
func (h *backgroundHost) HeartbeatAttemptTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope) error {
	return h.ledger.HeartbeatAttemptTx(ctx, tx, scope)
}
func (h *backgroundHost) CheckAttemptBudget(ctx context.Context, scope background.TaskScope) error {
	return h.ledger.CheckAttemptBudget(ctx, scope)
}

func (h *backgroundHost) transitionTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, before, after *bt.Task) error {
	if before.Status == bt.StatusPending && after.Status == bt.StatusRunning && after.CancelRequestedAt == nil {
		if err := h.checkResourceTx(ctx, tx, scope.Binding.TaskID); err != nil {
			return err
		}
	}
	if err := h.specs.TransitionTx(ctx, tx, scope, before, after); err != nil {
		return err
	}
	return h.interactions.TransitionTx(ctx, tx, scope, before, after)
}

// CheckEffectTx is called inside budget reservation/dispatch transactions.
func (h *backgroundHost) CheckEffectTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) error {
	taskScope, err := background.TaskScopeForBudgetTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	return h.checkAttemptTx(ctx, tx, taskScope, false)
}

func (h *backgroundHost) checkAttemptTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, allowStopping bool) error {
	if !allowStopping {
		if err := h.service.CheckEffectTx(ctx, tx, scope); err != nil {
			return err
		}
		if err := h.checkResourceRecordForTask(ctx, tx, scope.Binding.TaskID, true); err != nil {
			return err
		}
		return h.specs.CheckAttemptPolicyTx(ctx, tx, scope)
	}
	if err := h.store.Tasks().CheckAttemptTx(ctx, tx, scope.Binding.TaskID, scope.Attempt, true); err != nil {
		return err
	}
	var taskID string
	var attempt int64
	if err := tx.QueryRowContext(ctx, "SELECT task_id,attempt FROM harness_background_child_leases WHERE child_session_id=?", scope.Binding.ChildSessionID).Scan(&taskID, &attempt); err != nil {
		return err
	}
	if taskID != scope.Binding.TaskID || attempt != scope.Attempt {
		return bt.ErrLeaseLost
	}
	return nil
}

func (h *backgroundHost) Open(ctx context.Context, scope background.TaskScope) (*background.Attempt, error) {
	req, spec, err := h.specs.Load(ctx, scope.Binding)
	if err != nil {
		return nil, err
	}
	if err = h.Validate(ctx, scope.Binding); err != nil {
		return nil, err
	}
	ctx, resourceAttempt, finishResources, err := h.openResources(ctx, scope.Binding, spec)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(budget.WithScope(ctx, background.BudgetScope(scope)))
	broker, err := h.interactions.OpenAttempt(ctx, scope, h.checkAttemptTx)
	if err != nil {
		cancel()
		finishResources()
		return nil, err
	}
	hooks := broker.Hooks()
	ctx = interaction.WithExecutionHooks(ctx, hooks)
	var prepared *einoengine.PreparedAttempt
	if spec.Role != nil {
		engine := h.engine
		if spec.Roles != nil {
			engine, err = engine.WithRoleCatalog(spec.Roles)
			if err != nil {
				cancel()
				finishResources()
				return nil, err
			}
		}
		if spec.Version >= 4 {
			placement := einoengine.RolePlacement{Depth: spec.Depth}
			if spec.Version >= 5 {
				placement.ParentID = spec.ParentInvocationID
			}
			if spec.ParentTaskID != "" {
				if spec.Version == 4 {
					placement.ParentID = "background/" + spec.ParentTaskID
				}
			}
			if spec.Continuation != nil {
				var roleID string
				roleID, err = h.specs.BackgroundContinuationRole(ctx, scope.Binding, spec)
				if err == nil {
					prepared, err = engine.PrepareRestoredRoleAttempt(ctx, req, scope.Binding.AgentVersion, spec.Extension, *spec.Role, placement, roleID, broker.Publish, nil, cancel)
				}
			} else {
				prepared, err = engine.PreparePlacedRoleAttempt(ctx, req, scope.Binding.AgentVersion, spec.Extension, *spec.Role, placement, broker.Publish, nil, cancel)
			}
		} else {
			prepared, err = engine.PrepareRoleAttempt(ctx, req, scope.Binding.AgentVersion, spec.Extension, *spec.Role, broker.Publish, nil, cancel)
		}
	} else {
		prepared, err = h.engine.PrepareLegacyAttempt(ctx, req, scope.Binding.AgentVersion, spec.Extension, broker.Publish, nil, cancel)
	}
	if prepared == nil {
		cancel()
		finishResources()
		return nil, err
	}
	var executionErr error
	attempt := &background.Attempt{JoinAndClose: func(cleanup context.Context) error {
		defer cancel()
		defer finishResources()
		var joinErr error
		executionErr, joinErr = prepared.JoinAndCloseDetailed(cleanup)
		if h.assets != nil {
			joinErr = errors.Join(joinErr, h.assets.AbortRun(req.RunID))
		}
		return joinErr
	}, ExecutionFailure: func() error { return executionErr }}
	if resourceAttempt != nil {
		attempt.HostControls = resourceAttempt.controls
	}
	if err != nil {
		return attempt, err
	}
	if err = h.specs.PinEngineContract(ctx, scope, prepared.Contract, func(ctx context.Context, tx *sql.Tx, scope background.TaskScope) error {
		return h.checkAttemptTx(ctx, tx, scope, false)
	}); err != nil {
		return attempt, err
	}
	var nativeCancel atomic.Bool
	attempt.ObserveControl = func(control bt.ControlRequest) {
		switch control.Kind {
		case bt.ControlDrain, bt.ControlStop, bt.ControlTimeout:
			nativeCancel.Store(true)
		}
	}
	attempt.Agent = prepared.ObserveAgentWithTaskWaits(einoengine.BindExecutionAttempt(prepared.Agent, hooks, background.BudgetScope(scope)), broker.StageInterrupts, broker.StageTaskWaits, nativeCancel.Load)
	return attempt, nil
}

var _ background.AttemptFactory = (*backgroundHost)(nil)
var _ background.BudgetLedger = (*backgroundHost)(nil)
var _ background.HeartbeatBudgetLedger = (*backgroundHost)(nil)
var _ background.BudgetMonitor = (*backgroundHost)(nil)
