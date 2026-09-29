package deerflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// backgroundHost rebuilds every native attempt from durable child-owned state.
// It is process-owned; no foreground tool/provider closure is registered with
// the native task manager. Public assembly enables it only with the full broker.
type backgroundHost struct {
	store        *sqlite.Store
	runtime      *hr.Service
	engine       *einoengine.Engine
	assets       *assets.Store
	mcp          *mcp.Manager
	specs        *hr.BackgroundExecutionStore
	interactions *hr.BackgroundInteractionStore
	ledger       *background.LedgerAdapter
	service      *background.Service
	policy       string
}

type backgroundSpecKey struct{}

func backgroundHostPolicy(cfg Config) (string, error) {
	// Provider credentials stay process-local. Their presence is neither an
	// execution grant nor part of an immutable checkpoint resource identity.
	data, err := json.Marshal(struct {
		Version                               int
		Provider, BaseURL, Model, Instruction string
		MaxIterations                         int
		DisableSubagents                      bool
		Media                                 harness.MediaConfig
	}{1, cfg.Provider, cfg.BaseURL, cfg.Model, cfg.Instruction, cfg.MaxIterations, cfg.DisableSubagents, cfg.Media})
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
	if h.policy, err = backgroundHostPolicy(cfg); err != nil {
		return nil, err
	}
	runtime.ContinuationHostPolicy = h.policy
	if h.specs, err = hr.NewBackgroundExecutionStore(ctx, runtime.Store); err != nil {
		return nil, err
	}
	if h.interactions, err = hr.NewBackgroundInteractionStore(ctx, runtime.Store, h); err != nil {
		return nil, err
	}
	h.interactions.Assets = assets
	if h.ledger, err = background.NewLedgerAdapter(store, ledger); err != nil {
		return nil, err
	}
	h.service, err = background.New(ctx, background.Config{
		Store: store, Authorizer: h, Budgets: h, Approvals: h.interactions, Attempts: h,
		AgentNames: []string{backgroundAgentVersion}, MaxWorkers: workers,
		HeartbeatInterval: min(5*time.Second, h.ledger.HeartbeatInterval()),
		OnTransitionTx:    h.transitionTx,
	})
	if err != nil {
		return nil, err
	}
	return h, nil
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
	if spec.HostPolicy != h.policy || binding.AgentVersion != backgroundAgentVersion || spec.AgentVersion != backgroundAgentVersion || binding.ExecutionContract != contract {
		return harness.ErrTaskOriginConflict
	}
	var extension extensionState
	if err = json.Unmarshal(spec.Extension, &extension); err != nil || extension.Version != 1 || extension.MCPGeneration != "" {
		return harness.ErrBackgroundUnavailable
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
	ctx, cancel := context.WithCancel(budget.WithScope(ctx, background.BudgetScope(scope)))
	broker, err := h.interactions.OpenAttempt(ctx, scope, h.checkAttemptTx)
	if err != nil {
		cancel()
		return nil, err
	}
	hooks := broker.Hooks()
	ctx = interaction.WithExecutionHooks(ctx, hooks)
	prepared, err := h.engine.PrepareAttempt(ctx, req, scope.Binding.AgentVersion, spec.Extension, broker.Publish, nil, cancel)
	if prepared == nil {
		cancel()
		return nil, err
	}
	var executionErr error
	attempt := &background.Attempt{JoinAndClose: func(cleanup context.Context) error {
		defer cancel()
		var joinErr error
		executionErr, joinErr = prepared.JoinAndCloseDetailed(cleanup)
		if h.assets != nil {
			joinErr = errors.Join(joinErr, h.assets.AbortRun(req.RunID))
		}
		return joinErr
	}, ExecutionFailure: func() error { return executionErr }}
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
	attempt.Agent = prepared.ObserveAgentWithNativeCancel(einoengine.BindExecutionHooks(prepared.Agent, hooks), broker.StageInterrupts, nativeCancel.Load)
	return attempt, nil
}

var _ background.AttemptFactory = (*backgroundHost)(nil)
var _ background.BudgetLedger = (*backgroundHost)(nil)
var _ background.HeartbeatBudgetLedger = (*backgroundHost)(nil)
var _ background.BudgetMonitor = (*backgroundHost)(nil)
