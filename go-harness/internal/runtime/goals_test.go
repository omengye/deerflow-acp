package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestGoalCommandsAndCompletionRequireDurableSuccess(t *testing.T) {
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
	fail := true
	engine := testEngine(func(ctx context.Context, r harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		a, err := store.GoalForRun(ctx, r)
		if err != nil {
			return harness.RunResult{}, err
		}
		if a == nil {
			t.Fatal("goal not loaded")
		}
		if err = store.RecordGoalEvaluation(ctx, r, *a, harness.GoalEvaluation{Status: "achieved", Reason: "verified", Evidence: []string{"answer"}}, "evidence", false); err != nil {
			return harness.RunResult{}, err
		}
		if fail {
			return harness.RunResult{}, errors.New("cleanup failure")
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	s := NewService(store, engine, "test")
	x, err := s.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(text string) error {
		_, e := s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: text}}, nil, nil)
		return e
	}
	if err = run("/goal Verify output"); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_runs`).Scan(&count)
	if count != 0 {
		t.Fatal("goal command invoked model")
	}
	if err = run("start"); err == nil {
		t.Fatal("expected failed run")
	}
	g, err := store.Goal(ctx, x.ID)
	if err != nil || g.Status != "active" {
		t.Fatalf("%+v %v", g, err)
	}
	fail = false
	if err = run("verify"); err != nil {
		t.Fatal(err)
	}
	g, err = store.Goal(ctx, x.ID)
	if err != nil || g.Status != "achieved" {
		t.Fatalf("%+v %v", g, err)
	}
	reloaded, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	g, err = reloaded.Goal(ctx, x.ID)
	if err != nil || g.Status != "achieved" {
		t.Fatal("goal lost after reopen", g, err)
	}
	if err = run("/goal clear"); err != nil {
		t.Fatal(err)
	}
	g, err = store.Goal(ctx, x.ID)
	if err != nil || g != nil {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestGoalEvaluationCannotOverrideNativeReceiptBlockers(t *testing.T) {
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
	for _, state := range []harness.ReceiptState{harness.ReceiptPending, harness.ReceiptStarted, harness.ReceiptUncertain, harness.ReceiptFailed, harness.ReceiptNotExecuted, harness.ReceiptNoEffect} {
		t.Run(string(state), func(t *testing.T) {
			stopped := errors.New("stop after blocked evaluation")
			engine := testEngine(func(ctx context.Context, r harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				a, err := store.GoalForRun(ctx, r)
				if err != nil || a == nil {
					t.Fatal("goal not loaded", err)
				}
				receipt := harness.ToolReceipt{State: state}
				if state == harness.ReceiptNoEffect {
					receipt.Error = "read-only tool failed"
				}
				raw, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = native.DB().ExecContext(ctx, `INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,'blocked',?,?,1,?)`, r.RunID, r.Session.ID, state, raw); err != nil {
					t.Fatal(err)
				}
				for _, continued := range []bool{false, true} {
					verdict := harness.GoalEvaluation{Status: "achieved", Reason: "Optimistic judgment", Evidence: []string{"answer"}}
					if continued {
						verdict.Status, verdict.Next = "goal_not_met_yet", "Try again"
					}
					if err = store.RecordGoalEvaluation(ctx, r, *a, verdict, "progress", continued); !errors.Is(err, harness.ErrReconciliationRequired) {
						t.Fatalf("continued=%v bypassed %s receipt: %v", continued, state, err)
					}
				}
				var unchanged bool
				if err = native.DB().QueryRowContext(ctx, `SELECT evaluation IS NULL AND continuations=0 FROM harness_goal_runs WHERE run_id=?`, r.RunID).Scan(&unchanged); err != nil || !unchanged {
					t.Fatal("blocked evaluation changed durable goal state", err)
				}
				return harness.RunResult{}, stopped
			})
			s := NewService(store, engine, "test")
			x, err := s.NewSession(ctx, "owner", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "/goal Verify effects"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "start"}}, nil, nil); !errors.Is(err, stopped) {
				t.Fatal(err)
			}
			g, err := store.Goal(ctx, x.ID)
			if err != nil || g == nil || g.Status != "active" {
				t.Fatalf("goal completed across a blocker: %+v %v", g, err)
			}
		})
	}
}
