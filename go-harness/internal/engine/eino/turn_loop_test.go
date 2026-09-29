package eino

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestTurnLoopResumeRejectsPolicyChangeWithoutLosingCheckpoint(t *testing.T) {
	started := make(chan struct{})
	fake := &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call > 0 {
			return textStream("resumed"), nil
		}
		r, w := schema.Pipe[*schema.Message](1)
		go func() { defer w.Close(); close(started); <-ctx.Done(); w.Send(nil, ctx.Err()) }()
		return r, nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	req := request("versioned")
	req.Session.ConfigVersion = 7
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.Run(ctx, req, nil, nil); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("model did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	resume := request("resuming")
	resume.Session.ConfigVersion = 8
	if _, err := e.Resume(context.Background(), resume, CheckpointID(req.RunID), nil, nil); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("changed policy accepted: %v", err)
	}
	if fake.calls != 1 {
		t.Fatal("model ran despite changed policy")
	}
	resume.Session.ConfigVersion = 7
	if _, err := e.Resume(context.Background(), resume, CheckpointID(req.RunID), nil, nil); err != nil {
		t.Fatalf("valid resume after rejected attempt: %v", err)
	}
	if _, found, err := e.config.CheckpointStore.Get(context.Background(), CheckpointID(req.RunID)); err != nil || found {
		t.Fatalf("consumed checkpoint remains: found=%v err=%v", found, err)
	}
}

func TestTurnLoopMissingAndLegacyCheckpointDoNotStartModel(t *testing.T) {
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		t.Error("model started")
		return textStream("bad"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	if _, err := e.Resume(context.Background(), request("missing"), CheckpointID("absent"), nil, nil); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("missing checkpoint=%v", err)
	}
	if _, err := e.Resume(context.Background(), request("legacy"), "harness/run/raw-runner", nil, nil); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("legacy checkpoint=%v", err)
	}
}

type failingDeleteCheckpoints struct {
	*memoryCheckpoints
	failure error
}

func (s *failingDeleteCheckpoints) Delete(context.Context, string) error { return s.failure }

func TestTurnLoopDoesNotHideCheckpointCleanupFailure(t *testing.T) {
	failure := errors.New("checkpoint deletion failed")
	store := &failingDeleteCheckpoints{memoryCheckpoints: &memoryCheckpoints{items: make(map[string][]byte)}, failure: failure}
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("done"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, CheckpointStore: store})
	if _, err := e.Run(context.Background(), request("delete-error"), nil, nil); !errors.Is(err, failure) {
		t.Fatalf("checkpoint failure hidden: %v", err)
	}
}

func canceledCheckpoint(t *testing.T, e *Engine, started <-chan struct{}, runID string) []byte {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Run(ctx, request(runID), nil, allowTool)
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("execution ended before cancellation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not join execution")
	}
	data, found, err := e.config.CheckpointStore.Get(context.Background(), CheckpointID(runID))
	if err != nil || !found || len(data) == 0 {
		t.Fatalf("checkpoint found=%v bytes=%d err=%v", found, len(data), err)
	}
	return data
}

func waitingModel(started chan struct{}, next func(context.Context) (*schema.StreamReader[*schema.Message], error)) *scriptedModel {
	return &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call > 0 {
			return next(ctx)
		}
		r, w := schema.Pipe[*schema.Message](1)
		go func() {
			defer w.Close()
			close(started)
			<-ctx.Done()
			w.Send(nil, ctx.Err())
		}()
		return r, nil
	}}
}

// This mirrors only the native outer gob schema for corruption injection. The
// production adapter never decodes or depends on Eino's private runner schema.
type nativeTurnCheckpointFixture struct {
	RunnerCheckpointID string
	RunnerCheckpoint   []byte
	HasRunnerState     bool
	UnhandledItems     []turnItem
	ResumeItems        []turnItem
	CanceledItems      []turnItem
}

func TestTurnLoopCorruptInnerRunnerPreservesCheckpoint(t *testing.T) {
	started := make(chan struct{})
	fake := waitingModel(started, func(context.Context) (*schema.StreamReader[*schema.Message], error) {
		t.Error("model started with corrupt runner state")
		return textStream("unexpected"), nil
	})
	e := newTestEngine(t, Config{ChatModel: fake})
	data := canceledCheckpoint(t, e, started, "inner-corruption")
	saved, err := decodeCheckpointEnvelope(data)
	if err != nil {
		t.Fatal(err)
	}
	var native nativeTurnCheckpointFixture
	if err := gob.NewDecoder(bytes.NewReader(saved.Native)).Decode(&native); err != nil {
		t.Fatal(err)
	}
	if !native.HasRunnerState || len(native.CanceledItems) != 1 {
		t.Fatal("fixture does not contain an interrupted native turn")
	}
	native.RunnerCheckpoint = []byte("corrupt native runner bytes")
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(native); err != nil {
		t.Fatal(err)
	}
	saved.Native = encoded.Bytes()
	corrupt, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	key := CheckpointID("inner-corruption")
	if err := e.config.CheckpointStore.Set(context.Background(), key, corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(context.Background(), request("retry-inner"), key, nil, nil); err == nil {
		t.Fatal("corrupt runner checkpoint was accepted")
	}
	current, found, err := e.config.CheckpointStore.Get(context.Background(), key)
	if err != nil || !found || !bytes.Equal(current, corrupt) || fake.calls != 1 {
		t.Fatalf("corrupt checkpoint changed: found=%v modelCalls=%d err=%v", found, fake.calls, err)
	}
}

type unreliableSessionStore struct {
	adk.SessionEventStore[*schema.Message]
	failure error
	fail    atomic.Bool
}

func (s *unreliableSessionStore) LoadEvents(ctx context.Context, id string, req *adk.LoadSessionEventsRequest) (*adk.LoadSessionEventsResult[*schema.Message], error) {
	if s.fail.Load() {
		return nil, s.failure
	}
	return s.SessionEventStore.LoadEvents(ctx, id, req)
}

func TestTurnLoopSessionLoadFailureAllowsRetry(t *testing.T) {
	failure := errors.New("session storage temporarily unavailable")
	sessions := &unreliableSessionStore{SessionEventStore: einosession.NewInMemoryStore[*schema.Message](nil), failure: failure}
	started := make(chan struct{})
	fake := waitingModel(started, func(context.Context) (*schema.StreamReader[*schema.Message], error) {
		return textStream("resumed"), nil
	})
	e := newTestEngine(t, Config{ChatModel: fake, SessionStore: sessions})
	previous := canceledCheckpoint(t, e, started, "session-failure")
	key := CheckpointID("session-failure")
	sessions.fail.Store(true)
	if _, err := e.Resume(context.Background(), request("failed-resume"), key, nil, nil); !errors.Is(err, failure) {
		t.Fatalf("session load error hidden: %v", err)
	}
	current, found, err := e.config.CheckpointStore.Get(context.Background(), key)
	if err != nil || !found || !bytes.Equal(current, previous) {
		t.Fatalf("session failure changed checkpoint: found=%v err=%v", found, err)
	}
	sessions.fail.Store(false)
	if _, err := e.Resume(context.Background(), request("successful-retry"), key, nil, nil); err != nil {
		t.Fatalf("resume after recovered session storage: %v", err)
	}
}

func TestTurnLoopResumeFailuresPreserveOriginalBytes(t *testing.T) {
	for _, cause := range []string{"provider", "provider_cancel", "sink", "provider_drain", "tool_cleanup", "extension_cleanup"} {
		t.Run(cause, func(t *testing.T) {
			failure := errors.New(cause + " failure")
			started := make(chan struct{})
			fake := waitingModel(started, func(context.Context) (*schema.StreamReader[*schema.Message], error) {
				if cause == "provider" {
					return nil, failure
				}
				if cause == "provider_cancel" {
					return nil, context.Canceled
				}
				if cause == "provider_drain" {
					r, w := schema.Pipe[*schema.Message](2)
					w.Send(nil, context.Canceled)
					w.Send(nil, failure)
					w.Close()
					return r, nil
				}
				return textStream("completed native execution"), nil
			})
			var failing atomic.Bool
			config := Config{ChatModel: fake}
			if cause == "tool_cleanup" {
				config.ToolFactory = func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
					return nil, func() error {
						if failing.Load() {
							return failure
						}
						return nil
					}, nil
				}
			}
			if cause == "extension_cleanup" {
				config.ExtensionFactory = func(context.Context, harness.RunRequest, json.RawMessage) (RunExtensions, error) {
					return RunExtensions{Cleanup: func() error {
						if failing.Load() {
							return failure
						}
						return nil
					}}, nil
				}
			}
			e := newTestEngine(t, config)
			previous := canceledCheckpoint(t, e, started, "resume-failure")
			failing.Store(true)
			var events harness.EventHandler
			if cause == "sink" {
				events = func(context.Context, harness.RunEvent) error { return failure }
			}
			key := CheckpointID("resume-failure")
			result, err := e.Resume(context.Background(), request("attempt"), key, events, nil)
			if cause == "provider_cancel" {
				if err != nil || result.StopReason != "cancelled" {
					t.Fatalf("cancelled provider: result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("resume failure lost: %v", err)
			}
			current, found, err := e.config.CheckpointStore.Get(context.Background(), key)
			if err != nil || !found || !bytes.Equal(current, previous) {
				t.Fatalf("failed resume changed checkpoint: found=%v err=%v", found, err)
			}
		})
	}
}

func TestTurnLoopResumeRestoresModelAndTokenBudgets(t *testing.T) {
	for _, resource := range []string{"model_calls", "tokens"} {
		t.Run(resource, func(t *testing.T) {
			started := make(chan struct{})
			limits := harness.BudgetLimits{MaxModelCalls: 1}
			if resource == "tokens" {
				limits = harness.BudgetLimits{MaxTokens: 20000, MaxOutputTokens: 10}
			}
			fake := &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call != 0 {
					t.Error("resume reset logical execution budget")
					return textStream("unexpected"), nil
				}
				r, w := schema.Pipe[*schema.Message](1)
				go func() {
					defer w.Close()
					w.Send(&schema.Message{Role: schema.Assistant, Content: "partial", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 19990, CompletionTokens: 5, TotalTokens: 19995}}}, nil)
					close(started)
					<-ctx.Done()
					w.Send(nil, ctx.Err())
				}()
				return r, nil
			}}
			e := newTestEngine(t, Config{ChatModel: fake, Budget: limits})
			previous := canceledCheckpoint(t, e, started, "budget-resume")
			saved, err := decodeCheckpointEnvelope(previous)
			if err != nil || saved.Budget.ModelCalls != 1 || saved.Budget.Tokens != 19995 {
				t.Fatalf("budget settled too early: saved=%+v err=%v", saved, err)
			}
			// Recreate the engine to rule out accidental in-memory counter reuse.
			e = newTestEngine(t, e.config)
			result, err := e.Resume(context.Background(), request("continued"), CheckpointID("budget-resume"), nil, nil)
			if err != nil || result.Limit != resource || fake.calls != 1 {
				t.Fatalf("result=%+v calls=%d err=%v", result, fake.calls, err)
			}
		})
	}
}

func TestTurnLoopResumeRestoresToolBudget(t *testing.T) {
	underlying := &blockingTool{started: make(chan struct{}), stopped: make(chan struct{})}
	e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{underlying}, Budget: harness.BudgetLimits{MaxToolCalls: 1}})
	previous := canceledCheckpoint(t, e, underlying.started, "tool-budget-resume")
	saved, err := decodeCheckpointEnvelope(previous)
	if err != nil || saved.Budget.ToolCalls != 1 {
		t.Fatalf("missing tool budget: saved=%+v err=%v", saved, err)
	}
	e = newTestEngine(t, e.config)
	result, err := e.Resume(context.Background(), request("tool-continued"), CheckpointID("tool-budget-resume"), nil, allowTool)
	if err != nil || result.Limit != "tool_calls" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCheckpointBudgetRestoresActiveElapsedTime(t *testing.T) {
	b := &runBudget{limits: harness.BudgetLimits{Timeout: time.Minute}, startedAt: time.Now()}
	if err := b.restore(budgetSnapshot{ModelCalls: 3, ToolCalls: 5, Tokens: 91, Elapsed: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if remaining := b.remainingTime(); remaining <= 39*time.Second || remaining > 40*time.Second {
		t.Fatalf("timeout budget reset: remaining=%v", remaining)
	}
	snapshot, err := b.snapshot()
	if err != nil || snapshot.ModelCalls != 3 || snapshot.ToolCalls != 5 || snapshot.Tokens != 91 || snapshot.Elapsed < 20*time.Second {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if err := b.restore(budgetSnapshot{Tokens: -1}); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("negative durable budget accepted: %v", err)
	}
}

func TestTurnLoopRepeatedCancellationKeepsUpdatedCheckpoint(t *testing.T) {
	started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	fake := &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 2 {
			return textStream("resumed twice"), nil
		}
		r, w := schema.Pipe[*schema.Message](1)
		go func() {
			defer w.Close()
			w.Send(&schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 10 * (call + 1)}}}, nil)
			close(started[call])
			<-ctx.Done()
			w.Send(nil, ctx.Err())
		}()
		return r, nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	previous := canceledCheckpoint(t, e, started[0], "twice-canceled")
	e = newTestEngine(t, e.config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	key := CheckpointID("twice-canceled")
	go func() { _, err := e.Resume(ctx, request("first-resume"), key, nil, nil); done <- err }()
	select {
	case <-started[1]:
	case err := <-done:
		t.Fatalf("resume ended before cancellation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("resumed model did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed model did not stop")
	}
	current, found, err := e.config.CheckpointStore.Get(context.Background(), key)
	if err != nil || !found || bytes.Equal(current, previous) {
		t.Fatalf("updated checkpoint missing: found=%v err=%v", found, err)
	}
	saved, err := decodeCheckpointEnvelope(current)
	if err != nil || saved.Budget.ModelCalls != 2 || saved.Budget.Tokens != 30 {
		t.Fatalf("repeated cancellation budget: saved=%+v err=%v", saved, err)
	}
	e = newTestEngine(t, e.config)
	if _, err := e.Resume(context.Background(), request("second-resume"), key, nil, nil); err != nil {
		t.Fatalf("resume after second cancellation: %v", err)
	}
	if _, found, err := e.config.CheckpointStore.Get(context.Background(), key); err != nil || found {
		t.Fatalf("completed checkpoint retained: found=%v err=%v", found, err)
	}
}
