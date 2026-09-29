package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type Service struct {
	Store       *Store
	Coordinator *session.Coordinator
	Engine      harness.Engine
	Background  harness.BackgroundController
	// Set by the host at construction; public requests cannot supply policy.
	ContinuationHostPolicy string
	PermissionMode         harness.PermissionMode
	Model                  string
	Settings               ConfigSettings
	Resources              SessionResources
	// SessionCleanup runs after a successful durable session purge while the
	// coordinator still holds its deletion fence.
	SessionCleanup func(string) error
	Assets         *assets.Store
	Media          harness.MediaConfig
	Memory         *memory.Store
	MemoryUserID   string
	mu             sync.Mutex
	decisions      map[string]harness.PermissionDecision
}

func NewService(store *Store, engine harness.Engine, model string) *Service {
	return &Service{Store: store, Engine: engine, Model: model, Coordinator: session.NewCoordinator(), decisions: make(map[string]harness.PermissionDecision)}
}

func (s *Service) NewSession(ctx context.Context, owner, cwd string, servers ...harness.MCPServer) (harness.Session, error) {
	cwd, err := session.NormalizeWorkspace(cwd)
	if err != nil {
		return harness.Session{}, err
	}
	x, err := s.Store.CreateSession(ctx, cwd, s.Model)
	if err != nil {
		return x, err
	}
	x.ApprovalMode = harness.ApprovalAsk
	x.Subagents = s.Settings.EnableSubagents && s.Settings.DefaultSubagents
	x.ConfigVersion = 1
	if err = s.Store.SaveConfig(ctx, x); err != nil {
		return x, errors.Join(err, s.discardNewSession(x.ID))
	}
	ctx, release, _, err := s.Coordinator.AttachAndBegin(ctx, x.ID, owner)
	if err != nil {
		return x, errors.Join(err, s.discardNewSession(x.ID))
	}
	err = s.bindResources(ctx, owner, x, servers)
	if err != nil {
		err = errors.Join(err, s.abandonBinding(owner, x.ID, release, true))
	} else {
		release()
	}
	return x, err
}

func (s *Service) Load(ctx context.Context, owner, id, cwd string, replay bool, emit harness.EventHandler, servers ...harness.MCPServer) (harness.Session, error) {
	if err := s.Store.requireForegroundSession(ctx, id); err != nil {
		return harness.Session{}, err
	}
	x, err := s.Store.Session(ctx, id)
	if err != nil {
		return x, err
	}
	cwd, err = session.NormalizeWorkspace(cwd)
	if err != nil {
		return x, err
	}
	if !session.SameWorkspace(x.CWD, cwd) {
		return x, fmt.Errorf("%w: session workspace cannot be changed", harness.ErrInvalidInput)
	}
	ctx, release, fresh, err := s.Coordinator.AttachAndBegin(ctx, id, owner)
	if err != nil {
		return x, err
	}
	defer func() {
		if err != nil && fresh {
			_ = s.abandonBinding(owner, id, release)
		} else {
			release()
		}
	}()
	if err = s.bindResources(ctx, owner, x, servers); err != nil {
		return x, err
	}
	s.clearDecisions(owner, id)
	if replay {
		cursor := ""
		for {
			var page harness.EventPage
			page, err = s.Store.HistoryPage(ctx, id, cursor, 128)
			if err != nil {
				return x, err
			}
			for _, e := range page.Events {
				if emit != nil {
					err = emit(ctx, e)
					if err != nil {
						return x, err
					}
				}
			}
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
		}
	}
	if err = s.Store.MarkOpen(ctx, id); err != nil {
		return x, err
	}
	return x, nil
}

type reservation struct {
	sessionID, owner string
	used             atomic.Bool
}
type reservationKey struct{}

// Admit must run in transport receive order, before asynchronous prompt dispatch.
func (s *Service) Admit(ctx context.Context, owner, id string) (context.Context, func(), error) {
	if err := s.Store.requireForegroundSession(ctx, id); err != nil {
		return nil, nil, err
	}
	ctx, release, err := s.Coordinator.Begin(ctx, id, owner)
	if err != nil {
		return nil, nil, err
	}
	return context.WithValue(ctx, reservationKey{}, &reservation{sessionID: id, owner: owner}), release, nil
}

func (s *Service) Run(ctx context.Context, owner, id string, input []harness.Content, emit harness.EventHandler, approve harness.PermissionHandler) (result harness.RunResult, runErr error) {
	// A cancel can arrive immediately after admission, before input reaches SQL
	// or the engine. It still completes the original ACP prompt normally.
	defer func() {
		if cancellationOnly(runErr) {
			result.StopReason, runErr = "cancelled", nil
		}
	}()
	if r, ok := ctx.Value(reservationKey{}).(*reservation); ok && r.sessionID == id && r.owner == owner {
		if !r.used.CompareAndSwap(false, true) {
			return harness.RunResult{}, harness.ErrBusy
		}
	} else {
		var release func()
		var err error
		ctx, release, err = s.Admit(ctx, owner, id)
		if err != nil {
			return harness.RunResult{}, err
		}
		defer release()
		ctx.Value(reservationKey{}).(*reservation).used.Store(true)
	}
	if len(input) == 0 {
		return harness.RunResult{}, fmt.Errorf("%w: prompt must contain content", harness.ErrInvalidInput)
	}
	if err := s.Store.requireForegroundSession(ctx, id); err != nil {
		return harness.RunResult{}, err
	}
	x, err := s.Store.Session(ctx, id)
	if err != nil {
		return harness.RunResult{}, err
	}
	actualCWD, cwdErr := session.NormalizeWorkspace(x.CWD)
	if cwdErr != nil {
		return harness.RunResult{}, cwdErr
	}
	if !session.SameWorkspace(actualCWD, x.CWD) {
		return harness.RunResult{}, fmt.Errorf("%w: workspace directory was replaced", harness.ErrInvalidInput)
	}
	if err = ctx.Err(); err != nil {
		return harness.RunResult{}, err
	}
	if s.Engine == nil {
		return harness.RunResult{}, fmt.Errorf("model engine is not configured")
	}
	if err = s.Store.requireNoWaitingExecution(ctx, id); err != nil {
		return harness.RunResult{}, err
	}
	if err = s.Store.requireReconciled(ctx, id); err != nil {
		return harness.RunResult{}, err
	}
	req := harness.RunRequest{Session: x, RunID: NewID(), InputID: NewID(), Input: input}
	if s.Store.BudgetLedger != nil {
		req.RootBudgetID = req.RunID
	}
	var prepared *assets.Prepared
	if s.Assets != nil {
		prepared, err = s.Assets.Prepare(ctx, x, input)
		if err != nil {
			return harness.RunResult{}, err
		}
		req.Input = prepared.Input
		for _, c := range req.Input {
			if c.Type == "image" && !s.Media.SupportsVision(x.Model) {
				return harness.RunResult{}, errors.Join(fmt.Errorf("%w: the selected model does not support image input", harness.ErrInvalidInput), prepared.Finish(false))
			}
		}
	} else {
		for _, c := range input {
			if c.Type != "text" || c.Data != "" || c.Asset != nil || c.URI != "" {
				return harness.RunResult{}, fmt.Errorf("%w: media storage is not configured", harness.ErrInvalidInput)
			}
		}
	}
	if s.executionEngine() != nil {
		return s.runDurableNew(ctx, owner, req, prepared, emit, approve)
	}
	if err = s.Store.BeginRun(ctx, req, prepared); err != nil {
		if prepared != nil {
			err = errors.Join(err, prepared.Finish(false))
		}
		return harness.RunResult{}, err
	}
	var budgetScopes []budget.Scope
	runCtx := context.WithValue(ctx, taskActorKey{}, harness.TaskActor{OwnerID: owner, SessionID: id})
	stopBudgetHeartbeat := func() error { return nil }
	if s.Store.BudgetLedger != nil {
		scope := foregroundBudgetScope(req)
		runCtx, stopBudgetHeartbeat = keepBudgetAlive(budget.WithScope(runCtx, scope), s.Store.BudgetLedger, scope)
		budgetScopes = []budget.Scope{scope}
	}
	// Once input is accepted, every exit must close its attempt. Engine.Run
	// returns only after I/O and resources join; asset staging is cleaned before
	// the terminal run and budget transaction commits.
	defer func() {
		if s.Assets != nil {
			runErr = errors.Join(runErr, s.Assets.AbortRun(req.RunID))
		}
		heartbeatErr := stopBudgetHeartbeat()
		if cancellationOnly(runErr) || errors.Is(ctx.Err(), context.Canceled) {
			result.StopReason = "cancelled"
			if cancellationOnly(runErr) {
				runErr = nil
			}
		}
		if result.StopReason == "" {
			result.StopReason = "end_turn"
		}
		runErr = errors.Join(runErr, applyBudgetHeartbeat(&result, heartbeatErr))
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if finishErr := s.Store.Finish(finishCtx, req.RunID, result.StopReason, runErr, budgetScopes...); finishErr != nil {
			runErr = errors.Join(runErr, &runPersistenceError{err: finishErr})
		}
	}()
	if prepared != nil {
		if err = prepared.Finish(true); err != nil {
			return harness.RunResult{}, err
		}
	}
	// Serializes concurrent tool callbacks and their event order; the transport
	// has a separate reader so pending permissions never block cancellation.
	var eventMu sync.Mutex
	publish := func(eventCtx context.Context, e harness.RunEvent) error {
		eventMu.Lock()
		defer eventMu.Unlock()
		e.SessionID, e.RunID = id, req.RunID
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(eventCtx), 10*time.Second)
		defer cancel()
		e, err := s.Store.Append(persistCtx, e, s.Assets)
		if err != nil {
			return err
		}
		if emit != nil {
			return emit(eventCtx, e)
		}
		return nil
	}
	permissions := func(pctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		p.ID, p.SessionID, p.RunID = NewID(), id, req.RunID
		p.ConfigVersion = x.ConfigVersion
		if !json.Valid(p.Arguments) {
			return harness.RejectOnce, fmt.Errorf("invalid tool arguments")
		}
		intent, err := json.Marshal(struct {
			Tool          string
			Args          json.RawMessage
			ConfigVersion int64
		}{p.ToolName, p.Arguments, p.ConfigVersion})
		if err != nil {
			return harness.RejectOnce, err
		}
		digest := sha256.Sum256(intent)
		key := owner + "/" + id + "/" + fmt.Sprintf("%x", digest)
		if err = s.Store.Approval(pctx, p); err != nil {
			return harness.RejectOnce, err
		}
		decision, cached := s.configuredPermission(x, p)
		if !cached {
			s.mu.Lock()
			decision, cached = s.decisions[key]
			s.mu.Unlock()
		}
		if !cached {
			decision = harness.RejectOnce
			if approve != nil {
				decision, err = approve(pctx, p)
			}
		}
		if err != nil || pctx.Err() != nil {
			decision = harness.PermissionCancelled
		}
		switch decision {
		case harness.AllowOnce, harness.AllowAlways, harness.RejectOnce, harness.RejectAlways, harness.PermissionCancelled:
		default:
			decision = harness.RejectOnce
			err = fmt.Errorf("invalid permission decision")
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(pctx), 10*time.Second)
		defer cancel()
		if saveErr := s.Store.Decide(persistCtx, p.ID, decision); saveErr != nil {
			return harness.RejectOnce, saveErr
		}
		if decision == harness.AllowAlways || decision == harness.RejectAlways {
			s.mu.Lock()
			s.decisions[key] = decision
			s.mu.Unlock()
		}
		return decision, err
	}
	result, runErr = s.Engine.Run(runCtx, req, publish, permissions)
	return result, runErr
}

// A joined cleanup/persistence failure must not disappear just because a
// cancellation occurred at the same time.
func cancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	if persistence, ok := err.(interface{ PersistenceFailure() bool }); ok && persistence.PersistenceFailure() {
		return false
	}
	if err == context.Canceled {
		return true
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		items := multi.Unwrap()
		if len(items) == 0 {
			return false
		}
		for _, item := range items {
			if !cancellationOnly(item) {
				return false
			}
		}
		return true
	}
	return cancellationOnly(errors.Unwrap(err))
}

func (s *Service) SetMode(ctx context.Context, owner, id, mode string) error {
	if mode != "default" && mode != "plan" {
		return fmt.Errorf("%w: unknown mode %q", harness.ErrInvalidInput, mode)
	}
	ctx, release, err := s.Coordinator.Begin(ctx, id, owner)
	if err != nil {
		return err
	}
	defer release()
	if err = s.Store.requireNoWaitingExecution(ctx, id); err != nil {
		return err
	}
	x, err := s.Store.Session(ctx, id)
	if err != nil {
		return err
	}
	x.Mode = mode
	x.ConfigVersion++
	if err = s.Store.SaveConfig(ctx, x); err != nil {
		return err
	}
	s.clearDecisions(owner, id)
	return nil
}
func (s *Service) Disconnect(ctx context.Context, owner string) error {
	s.clearDecisions(owner, "")
	var cleanup func(context.Context, string) error
	if s.Resources != nil {
		cleanup = func(ctx context.Context, id string) error { return s.Resources.Release(ctx, owner, id) }
	}
	err := s.Coordinator.DisconnectWithCleanup(ctx, owner, cleanup)
	if s.Resources != nil {
		if ctx.Err() != nil && err != nil {
			// Stop waiting for the caller, but preserve run cleanup before owner
			// retirement closes active/pending/retired MCP generations.
			go func() {
				_ = s.Coordinator.DisconnectWithCleanup(context.Background(), owner, cleanup)
				_ = s.releaseResourceOwner(owner)
			}()
		} else {
			err = errors.Join(err, s.releaseResourceOwner(owner))
		}
	}
	return err
}
