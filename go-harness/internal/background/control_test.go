package background

import (
	"context"
	"sync"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

type controlRuntimeFixture struct{ controls chan bt.ControlRequest }

func (r *controlRuntimeFixture) Controls() <-chan bt.ControlRequest { return r.controls }
func (*controlRuntimeFixture) EmitProgress(context.Context, string, []byte) (bt.ProgressEmission, error) {
	return bt.ProgressEmission{EventID: "preserved-runtime"}, nil
}
func (*controlRuntimeFixture) ReportTranscriptFailure(context.Context, error) error { return nil }

func TestObservedControlsRecordsBeforeForwardingAndPreservesRuntime(t *testing.T) {
	runtime := &controlRuntimeFixture{controls: make(chan bt.ControlRequest, 2)}
	want := []bt.ControlRequest{{Kind: bt.ControlDrain, Reason: "shutdown"}, {Kind: bt.ControlStop, Reason: "owner"}}
	var seen []bt.ControlRequest
	wrapped, stop := observeRuntimeControls(context.Background(), runtime, &Attempt{ObserveControl: func(c bt.ControlRequest) { seen = append(seen, c) }})
	defer stop()
	for i, control := range want {
		runtime.controls <- control
		select {
		case got := <-wrapped.Controls():
			if got != control || len(seen) != i+1 || seen[i] != control {
				t.Fatalf("unobserved or changed control: got=%+v seen=%+v", got, seen)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("control was not forwarded")
		}
	}
	progress, err := wrapped.EmitProgress(context.Background(), "event", nil)
	if err != nil || progress.EventID != "preserved-runtime" {
		t.Fatalf("runtime projection changed: %+v %v", progress, err)
	}
	close(runtime.controls)
	select {
	case _, ok := <-wrapped.Controls():
		if ok {
			t.Fatal("closed upstream produced another control")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream close was not propagated")
	}
}

func TestObservedControlsJoinWhileForwardingIsBlocked(t *testing.T) {
	for _, shutdown := range []string{"stop", "context"} {
		t.Run(shutdown, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runtime := &controlRuntimeFixture{controls: make(chan bt.ControlRequest, 1)}
			observed := make(chan struct{})
			wrapped, stop := observeRuntimeControls(ctx, runtime, &Attempt{ObserveControl: func(bt.ControlRequest) { close(observed) }})
			defer stop()
			runtime.controls <- bt.ControlRequest{Kind: bt.ControlDrain}
			select {
			case <-observed:
			case <-time.After(3 * time.Second):
				t.Fatal("control observer never ran")
			}
			if shutdown == "context" {
				cancel()
				select {
				case _, ok := <-wrapped.Controls():
					if ok {
						// The pending send can win the cancellation race. The
						// following receive must still close without stop().
						select {
						case _, ok = <-wrapped.Controls():
							if ok {
								t.Fatal("unexpected control after cancellation")
							}
						case <-time.After(3 * time.Second):
							t.Fatal("context cancellation did not close forwarding")
						}
					}
				case <-time.After(3 * time.Second):
					t.Fatal("context cancellation did not stop wrapper")
				}
			}
			done := make(chan struct{})
			go func() { stop(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("stop did not join blocked forwarding")
			}
			if _, ok := <-wrapped.Controls(); ok {
				t.Fatal("joined wrapper retained an undelivered control")
			}
			var joined sync.WaitGroup
			for range 3 {
				joined.Add(1)
				go func() { defer joined.Done(); stop() }()
			}
			joined.Wait()
		})
	}
}

func TestObservedControlsOptionalObserverAndNilSource(t *testing.T) {
	runtime := &controlRuntimeFixture{controls: make(chan bt.ControlRequest, 1)}
	for _, attempt := range []*Attempt{nil, {}} {
		wrapped, stop := observeRuntimeControls(context.Background(), runtime, attempt)
		if wrapped != runtime {
			t.Fatal("absent observer changed runtime")
		}
		stop()
	}
	nilSource := &controlRuntimeFixture{}
	wrapped, stop := observeRuntimeControls(context.Background(), nilSource, &Attempt{ObserveControl: func(bt.ControlRequest) { t.Error("nil source invoked observer") }})
	if wrapped != nilSource || wrapped.Controls() != nil {
		t.Fatal("nil source changed runtime")
	}
	stop()
}
