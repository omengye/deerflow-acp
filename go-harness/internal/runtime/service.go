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
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type Service struct {
	Store       *Store
	Coordinator *session.Coordinator
	Engine      harness.Engine
	Model       string
	mu          sync.Mutex
	decisions   map[string]harness.PermissionDecision
}

func NewService(store *Store, engine harness.Engine, model string) *Service {
	return &Service{Store: store, Engine: engine, Model: model, Coordinator: session.NewCoordinator(), decisions: make(map[string]harness.PermissionDecision)}
}

func (s *Service) NewSession(ctx context.Context, owner, cwd string) (harness.Session, error) {
	cwd, err := session.NormalizeWorkspace(cwd)
	if err != nil {
		return harness.Session{}, err
	}
	x, err := s.Store.CreateSession(ctx, cwd, s.Model)
	if err != nil {
		return x, err
	}
	_, err = s.Coordinator.Attach(x.ID, owner)
	return x, err
}

func (s *Service) Load(ctx context.Context, owner, id, cwd string, replay bool, emit harness.EventHandler) (harness.Session, error) {
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
	fresh, err := s.Coordinator.Attach(id, owner)
	if err != nil {
		return x, err
	}
	ctx, release, err := s.Coordinator.Begin(ctx, id, owner)
	if err != nil {
		return x, err
	}
	defer func() {
		release()
		if err != nil && fresh {
			_ = s.Coordinator.Detach(context.Background(), id, owner)
		}
	}()
	if replay {
		var events []harness.RunEvent
		events, err = s.Store.History(ctx, id)
		if err != nil {
			return x, err
		}
		for _, e := range events {
			if emit != nil {
				err = emit(ctx, e)
				if err != nil {
					return x, err
				}
			}
		}
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
	// Additional input types are enabled only after their persistence and model
	// adapters are wired. Never claim image support before that capability works.
	for _, c := range input {
		if c.Type != "text" {
			return harness.RunResult{}, fmt.Errorf("%w: unsupported prompt content %q", harness.ErrInvalidInput, c.Type)
		}
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
	req := harness.RunRequest{Session: x, RunID: NewID(), InputID: NewID(), Input: input}
	if err = s.Store.BeginRun(ctx, req); err != nil {
		return harness.RunResult{}, err
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
		e, err := s.Store.Append(persistCtx, e)
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
		if !json.Valid(p.Arguments) {
			return harness.RejectOnce, fmt.Errorf("invalid tool arguments")
		}
		intent, err := json.Marshal(struct {
			Tool string
			Args json.RawMessage
		}{p.ToolName, p.Arguments})
		if err != nil {
			return harness.RejectOnce, err
		}
		digest := sha256.Sum256(intent)
		key := owner + "/" + id + "/" + fmt.Sprintf("%x", digest)
		if err = s.Store.Approval(pctx, p); err != nil {
			return harness.RejectOnce, err
		}
		s.mu.Lock()
		decision, cached := s.decisions[key]
		s.mu.Unlock()
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
	result, runErr = s.Engine.Run(ctx, req, publish, permissions)
	if cancellationOnly(runErr) || errors.Is(ctx.Err(), context.Canceled) {
		result.StopReason = "cancelled"
		if cancellationOnly(runErr) {
			runErr = nil
		}
	}
	if result.StopReason == "" {
		result.StopReason = "end_turn"
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if finishErr := s.Store.Finish(finishCtx, req.RunID, result.StopReason, runErr); finishErr != nil {
		return result, errors.Join(runErr, finishErr)
	}
	return result, runErr
}

// A joined cleanup/persistence failure must not disappear just because a
// cancellation occurred at the same time.
func cancellationOnly(err error) bool {
	if err == nil {
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
	return s.Store.SetMode(ctx, id, mode)
}
func (s *Service) Disconnect(ctx context.Context, owner string) error {
	err := s.Coordinator.Disconnect(ctx, owner)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.decisions {
		if len(key) > len(owner) && key[:len(owner)+1] == owner+"/" {
			delete(s.decisions, key)
		}
	}
	return err
}
