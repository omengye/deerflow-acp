package eino

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type testGoalStore struct {
	attempt     harness.GoalAttempt
	evaluations []harness.GoalEvaluation
}

func (s *testGoalStore) GoalForRun(context.Context, harness.RunRequest) (*harness.GoalAttempt, error) {
	a := s.attempt
	return &a, nil
}
func (s *testGoalStore) RecordGoalEvaluation(_ context.Context, _ harness.RunRequest, a harness.GoalAttempt, v harness.GoalEvaluation, p string, continued bool) error {
	s.evaluations = append(s.evaluations, v)
	s.attempt = a
	s.attempt.LastProgress = p
	if continued {
		s.attempt.Continuations++
	}
	return nil
}
func goalStore() *testGoalStore {
	return &testGoalStore{attempt: harness.GoalAttempt{Goal: harness.Goal{ID: "goal-1", Objective: "Inspect the workspace and report verified results", Status: "active"}}}
}
func TestGoalNativeContinuationAndMeteredEvaluator(t *testing.T) {
	store := goalStore()
	target := &recordingTool{}
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return textStream("I will inspect it."), nil
		case 1:
			return textStream(`{"status":"goal_not_met_yet","reason":"Inspection is not performed","evidence":["answer"],"next":"Read the workspace once"}`), nil
		case 2:
			return limitedOutput("", "tool_calls", schema.ToolCall{ID: "read-1", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}), nil
		case 3:
			return textStream("Verified workspace result."), nil
		case 4:
			if !strings.Contains(input[1].Content, "tool:read-1") {
				t.Error("evaluator lost tool evidence")
			}
			return textStream(`{"status":"achieved","reason":"Tool evidence verifies the result","evidence":["tool:read-1"]}`), nil
		default:
			t.Errorf("unexpected call %d", call)
			return textStream("unexpected"), nil
		}
	}}
	e := newTestEngine(t, Config{ChatModel: fake, GoalStore: store, GoalAutoContinue: true, Tools: []tool.BaseTool{target}, Budget: harness.BudgetLimits{MaxModelCalls: 5}})
	result, err := e.Run(context.Background(), request("goal"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 5 || len(store.evaluations) != 2 || store.evaluations[1].Status != "achieved" || store.attempt.Continuations != 1 {
		t.Fatalf("result=%+v err=%v calls=%d store=%+v", result, err, fake.calls, store)
	}
}
func TestGoalStopsOnBlockerInvalidVerdictAndNoProgress(t *testing.T) {
	for _, verdict := range []string{`{"status":"needs_user_input","reason":"Need authorization","evidence":[]}`, `{"status":"external_wait","reason":"Waiting for deployment","evidence":[]}`, `{"status":"achieved","reason":"Done","evidence":["invented"]}`, `not json`, `{"status":"goal_not_met_yet","reason":"Need more work","evidence":["answer"],"next":"Inspect files"}`} {
		store := goalStore()
		fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
			if call%2 == 1 {
				return textStream(verdict), nil
			}
			return textStream("Not yet done."), nil
		}}
		e := newTestEngine(t, Config{ChatModel: fake, GoalStore: store, GoalAutoContinue: true})
		result, err := e.Run(context.Background(), request("blocker"), nil, nil)
		if err != nil || result.StopReason != "end_turn" || fake.calls > 4 || store.attempt.Continuations > 1 {
			t.Fatalf("%+v %v calls=%d", result, err, fake.calls)
		}
	}
}
func TestGoalEvaluatorCannotBypassModelBudget(t *testing.T) {
	store := goalStore()
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("Answer"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, GoalStore: store, GoalAutoContinue: true, Budget: harness.BudgetLimits{MaxModelCalls: 1}})
	result, err := e.Run(context.Background(), request("budget"), nil, nil)
	if err != nil || fake.calls != 1 || result.StopReason == "end_turn" || len(store.evaluations) != 0 {
		t.Fatalf("%+v %v calls=%d", result, err, fake.calls)
	}
}

func TestGoalBoundedEvenWhenToolsKeepProducingEvidence(t *testing.T) {
	store := goalStore()
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call % 3 {
		case 0:
			return limitedOutput("", "tool_calls", schema.ToolCall{ID: fmt.Sprintf("read-%d", call), Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}), nil
		case 1:
			return textStream("More work remains"), nil
		default:
			return textStream(`{"status":"goal_not_met_yet","reason":"Not finished","evidence":["answer"],"next":"Read more evidence"}`), nil
		}
	}}
	e := newTestEngine(t, Config{ChatModel: fake, GoalStore: store, GoalAutoContinue: true, Tools: []tool.BaseTool{&recordingTool{}}})
	result, err := e.Run(context.Background(), request("bounded"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 12 || store.attempt.Continuations != 3 {
		t.Fatalf("%+v %v calls=%d continuations=%d", result, err, fake.calls, store.attempt.Continuations)
	}
}

func TestGoalResumeRejectsChangedObjectiveAndKeepsCheckpoint(t *testing.T) {
	store := goalStore()
	started := make(chan struct{})
	fake := &scriptedModel{stream: func(ctx context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			r, w := schema.Pipe[*schema.Message](1)
			go func() { defer w.Close(); close(started); <-ctx.Done(); w.Send(nil, ctx.Err()) }()
			return r, nil
		}
		if call == 1 {
			return textStream("Completed a textual answer"), nil
		}
		return textStream(`{"status":"achieved","reason":"Answer meets the textual goal","evidence":["answer"]}`), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, GoalStore: store})
	req := request("goal-resume")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.Run(ctx, req, nil, nil); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not cancel")
	}
	old := store.attempt.Goal.Objective
	store.attempt.Goal.Objective = "A changed goal"
	if _, err := e.Resume(context.Background(), req, CheckpointID(req.RunID), nil, nil); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("changed goal not fenced", err)
	}
	store.attempt.Goal.Objective = old
	if _, err := e.Resume(context.Background(), req, CheckpointID(req.RunID), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGoalDisabledContinuationStillEvaluates(t *testing.T) {
	store := goalStore()
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return textStream("Pending work"), nil
		}
		return textStream(`{"status":"goal_not_met_yet","reason":"Needs work","evidence":["answer"],"next":"Continue"}`), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, GoalStore: store, GoalAutoContinue: false})
	if _, err := e.Run(context.Background(), request("manual"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 || store.attempt.Continuations != 0 {
		t.Fatal("auto continuation without opt-in", fake.calls)
	}
}
