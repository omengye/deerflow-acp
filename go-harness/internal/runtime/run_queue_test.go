package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type queuedRunResult struct {
	result harness.RunResult
	err    error
}

func queuedRun(t *testing.T, s *Service, ctx context.Context, owner string, session harness.Session) <-chan queuedRunResult {
	t.Helper()
	done := make(chan queuedRunResult, 1)
	go func() {
		result, err := s.Run(ctx, owner, session.ID, []harness.Content{{Type: "text", Text: "run"}}, nil, nil)
		done <- queuedRunResult{result: result, err: err}
	}()
	return done
}

func receiveQueued[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a run")
		var zero T
		return zero
	}
}

func waitForQueuedRun(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for s.runQueue.queued.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run did not enter the queue")
		}
		time.Sleep(time.Millisecond)
	}
}

func queueService(t *testing.T, max int, timeout time.Duration, engine harness.Engine) (*Service, *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServiceWithRunQueue(store, engine, "model", RunQueueConfig{MaxActiveRuns: max, QueueTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return s, native
}

func newQueuedSession(t *testing.T, s *Service, owner string) harness.Session {
	t.Helper()
	session, err := s.NewSession(context.Background(), owner, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestRunQueueLimitsConcurrentSessionsAndReleasesSlot(t *testing.T) {
	entered := make(chan string, 3)
	release := make(chan struct{}, 3)
	var active, peak atomic.Int32
	engine := testEngine(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for prior := peak.Load(); current > prior && !peak.CompareAndSwap(prior, current); prior = peak.Load() {
		}
		entered <- req.Session.ID
		select {
		case <-release:
			return harness.RunResult{StopReason: "end_turn"}, nil
		case <-ctx.Done():
			return harness.RunResult{}, ctx.Err()
		}
	})
	s, _ := queueService(t, 2, time.Second, engine)
	first := newQueuedSession(t, s, "first")
	second := newQueuedSession(t, s, "second")
	third := newQueuedSession(t, s, "third")
	firstDone := queuedRun(t, s, context.Background(), "first", first)
	secondDone := queuedRun(t, s, context.Background(), "second", second)
	_ = receiveQueued(t, entered)
	_ = receiveQueued(t, entered)
	thirdDone := queuedRun(t, s, context.Background(), "third", third)
	waitForQueuedRun(t, s)
	select {
	case id := <-entered:
		t.Fatalf("third run entered with both slots occupied: %s", id)
	case <-time.After(75 * time.Millisecond):
	}
	release <- struct{}{}
	if id := receiveQueued(t, entered); id != third.ID {
		t.Fatalf("unexpected queued session %s", id)
	}
	release <- struct{}{}
	release <- struct{}{}
	for _, done := range []<-chan queuedRunResult{firstDone, secondDone, thirdDone} {
		if result := receiveQueued(t, done); result.err != nil || result.result.StopReason != "end_turn" {
			t.Fatalf("run result: %+v", result)
		}
	}
	if peak.Load() != 2 || active.Load() != 0 {
		t.Fatalf("concurrency peak=%d active=%d", peak.Load(), active.Load())
	}
}

func TestRunQueueTimeoutLeavesQueuedSessionRetryable(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{}, 2)
	engine := testEngine(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		entered <- req.Session.ID
		select {
		case <-release:
			return harness.RunResult{StopReason: "end_turn"}, nil
		case <-ctx.Done():
			return harness.RunResult{}, ctx.Err()
		}
	})
	s, native := queueService(t, 1, 75*time.Millisecond, engine)
	first := newQueuedSession(t, s, "first")
	second := newQueuedSession(t, s, "second")
	firstDone := queuedRun(t, s, context.Background(), "first", first)
	if id := receiveQueued(t, entered); id != first.ID {
		t.Fatal("first run did not enter")
	}
	secondDone := queuedRun(t, s, context.Background(), "second", second)
	waitForQueuedRun(t, s)
	if outcome := receiveQueued(t, secondDone); !errors.Is(outcome.err, harness.ErrQueueTimeout) {
		t.Fatalf("queued run outcome: %+v", outcome)
	}
	var count int
	if err := native.DB().QueryRow(`SELECT COUNT(*) FROM harness_runs WHERE session_id=?`, second.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("timed out run was persisted: count=%d err=%v", count, err)
	}
	release <- struct{}{}
	if outcome := receiveQueued(t, firstDone); outcome.err != nil {
		t.Fatal(outcome.err)
	}
	retry := queuedRun(t, s, context.Background(), "second", second)
	if id := receiveQueued(t, entered); id != second.ID {
		t.Fatalf("retry entered wrong session: %s", id)
	}
	release <- struct{}{}
	if outcome := receiveQueued(t, retry); outcome.err != nil {
		t.Fatalf("queued session was not retryable: %v", outcome.err)
	}
}

func TestRunQueueCancellationDoesNotStartLater(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{}, 1)
	engine := testEngine(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		entered <- req.Session.ID
		select {
		case <-release:
			return harness.RunResult{StopReason: "end_turn"}, nil
		case <-ctx.Done():
			return harness.RunResult{}, ctx.Err()
		}
	})
	s, _ := queueService(t, 1, time.Second, engine)
	first := newQueuedSession(t, s, "first")
	second := newQueuedSession(t, s, "second")
	firstDone := queuedRun(t, s, context.Background(), "first", first)
	_ = receiveQueued(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := queuedRun(t, s, ctx, "second", second)
	waitForQueuedRun(t, s)
	cancel()
	if outcome := receiveQueued(t, secondDone); outcome.err != nil || outcome.result.StopReason != "cancelled" {
		t.Fatalf("cancelled queue wait: %+v", outcome)
	}
	release <- struct{}{}
	if outcome := receiveQueued(t, firstDone); outcome.err != nil {
		t.Fatal(outcome.err)
	}
	select {
	case id := <-entered:
		t.Fatalf("cancelled run entered later: %s", id)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestRunQueueConfigValidation(t *testing.T) {
	for _, cfg := range []RunQueueConfig{{}, {MaxActiveRuns: 129}, {MaxActiveRuns: 1, QueueTimeout: -time.Second}, {MaxActiveRuns: 1, QueueTimeout: 25 * time.Hour}} {
		if _, err := newRunQueue(cfg); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted invalid run queue %+v: %v", cfg, err)
		}
	}
}
