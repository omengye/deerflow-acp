package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type testEngine func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error)

func (f testEngine) Run(ctx context.Context, r harness.RunRequest, e harness.EventHandler, p harness.PermissionHandler) (harness.RunResult, error) {
	return f(ctx, r, e, p)
}

func TestRunDurabilityLoadAndResume(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "harness.db")
	native, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	engine := testEngine(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		var count int
		if err := native.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_inputs WHERE id=?`, r.InputID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatal("engine invoked before durable input")
		}
		return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "answer"})
	})
	s := NewService(store, engine, "test")
	workspace := t.TempDir()
	x, err := s.NewSession(ctx, "one", workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Run(ctx, "one", x.ID, []harness.Content{{Type: "text", Text: "question"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Disconnect(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err = native.Close(); err != nil {
		t.Fatal(err)
	}
	native, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err = NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	s = NewService(store, nil, "test")
	var replay []harness.RunEvent
	emit := func(_ context.Context, e harness.RunEvent) error { replay = append(replay, e); return nil }
	if _, err = s.Load(ctx, "two", x.ID, workspace, true, emit); err != nil {
		t.Fatal(err)
	}
	if len(replay) != 2 || replay[0].Kind != "user_message" || replay[1].Text != "answer" {
		t.Fatalf("history: %+v", replay)
	}
	if _, err = s.Load(ctx, "two", x.ID, workspace, false, emit); err != nil {
		t.Fatal(err)
	}
	if len(replay) != 2 {
		t.Fatal("resume replayed history")
	}
	if _, err = s.Load(ctx, "other", x.ID, workspace, false, nil); !errors.Is(err, harness.ErrAttachedElsewhere) {
		t.Fatal(err)
	}
}

func TestPermissionScopeAndCancellationPersistence(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	engine := testEngine(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, p harness.PermissionHandler) (harness.RunResult, error) {
		for i, arg := range []string{`{"path":"one"}`, `{"path":"one"}`, `{"path":"two"}`} {
			callID := fmt.Sprintf("call-%d", i)
			if err := emit(ctx, harness.RunEvent{Kind: "tool_start", ToolName: "write_file", ToolCallID: callID, Status: "pending", Arguments: []byte(arg)}); err != nil {
				return harness.RunResult{}, err
			}
			if _, err := p(ctx, harness.PermissionRequest{ToolName: "write_file", ToolCallID: callID, Arguments: []byte(arg)}); err != nil {
				return harness.RunResult{}, err
			}
			if err := emit(ctx, harness.RunEvent{Kind: "tool_execute", ToolName: "write_file", ToolCallID: callID, Status: "in_progress"}); err != nil {
				return harness.RunResult{}, err
			}
			if err := emit(ctx, harness.RunEvent{Kind: "tool_end", ToolName: "write_file", ToolCallID: callID, Status: "completed"}); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	s := NewService(store, engine, "test")
	x, err := s.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	approve := func(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		calls++
		var decision string
		if err := native.DB().QueryRowContext(ctx, `SELECT decision FROM harness_approvals WHERE id=?`, p.ID).Scan(&decision); err != nil {
			t.Fatal(err)
		}
		if decision != "pending" {
			t.Fatal(decision)
		}
		return harness.AllowAlways, nil
	}
	if _, err = s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "do it"}}, nil, approve); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls %d, want 2 exact argument scopes", calls)
	}
	if err = s.Disconnect(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(ctx, "new", x.ID, x.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Run(ctx, "new", x.ID, []harness.Content{{Type: "text", Text: "again"}}, nil, approve); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatal("old connection decisions survived")
	}
}

func TestCallbackCannotReuseRunAdmission(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	engineCalls := 0
	engine := testEngine(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		engineCalls++
		return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "hello"})
	})
	s := NewService(store, engine, "test")
	x, err := s.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := []harness.Content{{Type: "text", Text: "go"}}
	_, err = s.Run(ctx, "owner", x.ID, input, func(callbackCtx context.Context, _ harness.RunEvent) error {
		_, nestedErr := s.Run(callbackCtx, "owner", x.ID, input, nil, nil)
		if !errors.Is(nestedErr, harness.ErrBusy) {
			t.Fatalf("reused admission: %v", nestedErr)
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if engineCalls != 1 {
		t.Fatalf("engine entered %d times", engineCalls)
	}
}

func TestCancelPreservesCleanupFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("cleanup failed")
	engine := testEngine(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		cancel()
		return harness.RunResult{StopReason: "cancelled"}, errors.Join(context.Canceled, failure)
	})
	s := NewService(store, engine, "test")
	x, err := s.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "go"}}, nil, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("lost cleanup failure: %v", err)
	}
	var status string
	if err = native.DB().QueryRow(`SELECT status FROM harness_runs`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("persisted status %q", status)
	}
}
