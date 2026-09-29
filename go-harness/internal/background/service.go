package background

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	ds "github.com/cloudwego/eino/adk/backgroundtask/subagent"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type Service struct {
	config                Config
	store                 *sqlite.Store
	tasks                 *sqlite.TaskStore
	manager               *bt.Manager
	registry              *bt.ExecutorRegistry
	mu                    sync.Mutex
	attempts              map[string]*attemptState
	inflight              map[string]bool
	backoff               map[string]time.Time
	closed, started       bool
	dispatchCancel        context.CancelFunc
	scanDone, workersDone chan struct{}
	jobs                  chan string
	wakeup                chan struct{}
	workers               sync.WaitGroup
	cleanupErr            error
}

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("background: SQLite store is required")
	}
	if len(cfg.AgentNames) > 0 && cfg.Attempts == nil {
		return nil, errors.New("background: durable agents require an attempt factory")
	}
	if cfg.MaxWorkers == 0 {
		cfg.MaxWorkers = 4
	}
	if cfg.MaxWorkers < 1 || cfg.MaxWorkers > 64 {
		return nil, errors.New("background: worker limit must be 1..64")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.NotificationLease <= 0 {
		cfg.NotificationLease = 30 * time.Second
	}
	if cfg.MaxCheckpointBytes <= 0 {
		cfg.MaxCheckpointBytes = 8 << 20
	}
	if cfg.DrainCancelTimeout <= 0 {
		cfg.DrainCancelTimeout = 5 * time.Second
	}
	s := &Service{config: cfg, store: cfg.Store, registry: bt.NewExecutorRegistry(), attempts: map[string]*attemptState{}, inflight: map[string]bool{}, backoff: map[string]time.Time{}, wakeup: make(chan struct{}, 1), scanDone: make(chan struct{}), workersDone: make(chan struct{}), jobs: make(chan string, cfg.MaxWorkers)}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	s.tasks = cfg.Store.Tasks().WithHooks(sqlite.TaskHooks{Create: s.onCreate, Transition: s.onTransition})
	var err error
	s.manager, err = bt.New(ctx, &bt.Config{Tasks: s.tasks, TaskEvents: s.tasks, Executors: s.registry, HeartbeatInterval: func() time.Duration { return cfg.HeartbeatInterval }, SendTaskCreatedEvent: s.sendCreated})
	if err != nil {
		return nil, err
	}
	for _, executor := range cfg.AdditionalExecutors {
		if executor == nil {
			return nil, errors.New("background: nil executor")
		}
		if err = s.registry.Register(&managedExecutor{service: s, inner: executor}); err != nil {
			return nil, err
		}
	}
	if len(cfg.AgentNames) > 0 {
		native, err := ds.NewExecutor(&ds.ExecutorConfig[*schema.Message]{SessionStoreFactory: s.childStore, CheckPointStore: checkpointDispatcher{service: s}, DrainCancelTimeout: cfg.DrainCancelTimeout})
		if err != nil {
			return nil, err
		}
		for _, name := range cfg.AgentNames {
			if err = native.Register(name, &ds.AgentRegistration[*schema.Message]{Agent: &agentProxy{name: name}}); err != nil {
				return nil, err
			}
		}
		if err = s.registry.Register(&managedExecutor{service: s, inner: native}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Service) sendCreated(ctx context.Context, task *bt.Task) error {
	if sessionID, ok := adk.RunnerSessionID(ctx); ok && sessionID == task.Spec.SessionID {
		return bt.TaskCreatedSessionEventSender[*schema.Message]()(ctx, task)
	}
	// SDK/worker submissions have no active parent Runner. The atomic outbox is
	// their authoritative delivery route; no concurrent parent log writer here.
	return nil
}

type createContextKey struct{}

func (s *Service) Submit(ctx context.Context, actor harness.TaskActor, in Submission) (harness.BackgroundTask, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return harness.BackgroundTask{}, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return harness.BackgroundTask{}, harness.ErrBackgroundClosed
	}
	if s.config.Budgets == nil {
		return harness.BackgroundTask{}, harness.ErrBackgroundUnavailable
	}
	in, err := prepareSubmission(actor, in)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	if existing, _, loadErr := loadBinding(ctx, s.store.DB(), in.Binding.TaskID); loadErr == nil {
		if existing.IntentHash != in.Binding.IntentHash {
			return harness.BackgroundTask{}, harness.ErrTaskOriginConflict
		}
		return s.Get(ctx, actor, existing.TaskID)
	} else if !errors.Is(loadErr, harness.ErrNotFound) {
		return harness.BackgroundTask{}, loadErr
	}
	if s.config.Attempts != nil {
		if err = s.config.Attempts.Validate(ctx, in.Binding); err != nil {
			return harness.BackgroundTask{}, err
		}
	}
	ctx = context.WithValue(ctx, createContextKey{}, in.Binding)
	task, submitErr := s.manager.Submit(ctx, &bt.SubmitRequest{Spec: in.Spec})
	if errors.Is(submitErr, bt.ErrAlreadyExists) {
		existing, _, loadErr := loadBinding(ctx, s.store.DB(), in.Binding.TaskID)
		if loadErr != nil {
			return harness.BackgroundTask{}, errors.Join(submitErr, loadErr)
		}
		if existing.IntentHash != in.Binding.IntentHash {
			return harness.BackgroundTask{}, harness.ErrTaskOriginConflict
		}
		return s.Get(ctx, actor, existing.TaskID)
	}
	if task == nil {
		return harness.BackgroundTask{}, submitErr
	}
	s.wake()
	return project(task, in.Binding, ""), submitErr // nonnil accepted result retains ownership on notification failure
}

// SubmitNativeSubagent uses Eino's public serializer; native payload bytes remain
// opaque. RequestHash makes repeated origin identity independently verifiable.
func (s *Service) SubmitNativeSubagent(ctx context.Context, actor harness.TaskActor, b Binding, input *adk.AgentInput, description string) (harness.BackgroundTask, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return harness.BackgroundTask{}, err
	}
	if s.config.Budgets == nil || b.AgentVersion == "" {
		return harness.BackgroundTask{}, harness.ErrBackgroundUnavailable
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return harness.BackgroundTask{}, harness.ErrBackgroundClosed
	}
	// Native Submit serializes its public request. A short-lived request scope
	// lets the transaction hook derive the final native Spec hash before create.
	if b.ParentSessionID != "" && b.ParentSessionID != actor.SessionID {
		return harness.BackgroundTask{}, harness.ErrPermissionDenied
	}
	b.ParentSessionID = actor.SessionID
	b.ExecutorKey = ds.ExecutorKey
	b.TaskID = originTaskID(b)
	if b.ChildSessionID == "" {
		b.ChildSessionID = b.TaskID + "/session"
	}
	data, hashErr := json.Marshal(struct {
		Input       *adk.AgentInput
		Description string
	}{input, description})
	if hashErr != nil {
		return harness.BackgroundTask{}, hashErr
	}
	b.RequestHash = fmt.Sprintf("%x", sha256.Sum256(data))
	if s.config.Attempts != nil {
		if err := s.config.Attempts.Validate(ctx, b); err != nil {
			return harness.BackgroundTask{}, err
		}
	}
	if _, _, err := loadBinding(ctx, s.store.DB(), b.TaskID); err == nil {
		return s.nativeReplay(ctx, actor, b)
	} else if !errors.Is(err, harness.ErrNotFound) {
		return harness.BackgroundTask{}, err
	}
	request := &nativeSubmission{actor: actor, binding: b}
	ctx = context.WithValue(ctx, nativeContextKey{}, request)
	task, err := ds.Submit(ctx, s.manager, &ds.SubmitRequest[*schema.Message]{TaskID: b.TaskID, SubAgentName: b.AgentVersion, Input: input, Description: description, SessionID: actor.SessionID, ChildSessionID: b.ChildSessionID})
	if errors.Is(err, bt.ErrAlreadyExists) {
		return s.nativeReplay(ctx, actor, b)
	}
	if task == nil {
		return harness.BackgroundTask{}, err
	}
	s.wake()
	binding, _, loadErr := loadBinding(ctx, s.store.DB(), task.Spec.ID)
	if loadErr != nil {
		return harness.BackgroundTask{}, errors.Join(err, loadErr)
	}
	return project(task, binding, ""), err
}

func (s *Service) nativeReplay(ctx context.Context, actor harness.TaskActor, b Binding) (harness.BackgroundTask, error) {
	old, err := s.manager.Get(ctx, b.TaskID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	binding, _, err := loadBinding(ctx, s.store.DB(), b.TaskID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	prepared, err := prepareSubmission(actor, Submission{Binding: b, Spec: old.Spec})
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	if binding.IntentHash != prepared.Binding.IntentHash {
		return harness.BackgroundTask{}, harness.ErrTaskOriginConflict
	}
	return s.Get(ctx, actor, b.TaskID)
}

type nativeContextKey struct{}
type nativeSubmission struct {
	actor   harness.TaskActor
	binding Binding
}

func (s *Service) report(err error) {
	if err != nil && s.config.OnError != nil {
		s.config.OnError(err)
	}
}
func (s *Service) wake() {
	select {
	case s.wakeup <- struct{}{}:
	default:
	}
}
func attemptKey(scope TaskScope) string {
	return fmt.Sprintf("%s/%d", scope.Binding.TaskID, scope.Attempt)
}

type agentProxy struct{ name string }

func (p *agentProxy) Name(context.Context) string        { return p.name }
func (p *agentProxy) Description(context.Context) string { return "Durable task-owned child agent" }
func (p *agentProxy) Run(ctx context.Context, input *adk.AgentInput, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	v, ok := ctx.Value(attemptContextKey{}).(*attemptContext)
	if !ok || v.attempt == nil || v.attempt.Agent == nil {
		return agentError(harness.ErrBackgroundUnavailable)
	}
	return v.attempt.Agent.Run(ctx, input, opts...)
}
func (p *agentProxy) Resume(ctx context.Context, info *adk.ResumeInfo, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	v, ok := ctx.Value(attemptContextKey{}).(*attemptContext)
	if !ok || v.attempt == nil || v.attempt.Agent == nil {
		return agentError(harness.ErrBackgroundUnavailable)
	}
	return v.attempt.Agent.Resume(ctx, info, opts...)
}
func agentError(err error) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() { defer gen.Close(); gen.Send(&adk.AgentEvent{Err: err}) }()
	return iter
}
