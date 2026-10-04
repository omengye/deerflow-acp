package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestAutomaticExecutionStartedBeforeEngineAndFailurePreventsWork(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "observer_failure"}[fail], func(t *testing.T) {
			f := newExecutionWaitFixture(t)
			f.suspend(t, false)
			f.complete(t)
			engine := &automaticWaitEngine{}
			service := automaticService(t, f, engine)
			var started atomic.Bool
			engine.onResume = func(context.Context, harness.RunRequest) error {
				if !started.Load() {
					return errors.New("engine ran before committed start notification")
				}
				return nil
			}
			observerErr := errors.New("fixture observer failed")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			type outcome struct {
				result harness.RunResult
				err    error
			}
			results := make(chan outcome, 1)
			go func() {
				done <- service.ServeSessionUpdates(ctx, "owner", f.req.Session.ID, harness.SessionUpdates{
					Started: func(ctx context.Context, state harness.ExecutionState) error {
						if state.SessionID != f.req.Session.ID || state.RunID != f.req.RunID || state.AttemptID == "" || state.Version < 1 || state.Status != harness.ExecutionResuming {
							return errors.New("invalid start identity")
						}
						persisted, err := f.s.Execution(ctx, state.SessionID, state.RunID)
						if err != nil || persisted.Version != state.Version || persisted.AttemptID != state.AttemptID {
							return errors.New("start preceded claim commit")
						}
						started.Store(true)
						if fail {
							return observerErr
						}
						return nil
					},
					Result: func(_ context.Context, result harness.RunResult, err error) error {
						results <- outcome{result, err}
						cancel()
						return nil
					},
				})
			}()
			select {
			case got := <-results:
				if fail {
					if !errors.Is(got.err, observerErr) || engine.resumes.Load() != 0 {
						t.Fatalf("observer failure dispatched work: %v %d", got.err, engine.resumes.Load())
					}
				} else if got.err != nil || engine.resumes.Load() != 1 || got.result.Execution == nil || got.result.Execution.Status != harness.ExecutionCompleted {
					t.Fatalf("automatic outcome: %+v %v", got.result, got.err)
				}
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("automatic lifecycle stalled")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("automatic lifecycle did not join")
			}
		})
	}
}
