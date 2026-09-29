package background

import (
	"context"
	"sync"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

type observedControlRuntime struct {
	bt.ExecutionRuntime
	controls <-chan bt.ControlRequest
}

func (r *observedControlRuntime) Controls() <-chan bt.ControlRequest { return r.controls }

// Observe a manager-owned control before the native executor can receive it.
// Native cancellation may surface as an opaque graph interrupt inside an agent;
// this signal lets the attempt distinguish it from an unsupported business
// interrupt. It conveys no permission grant or checkpoint state.
func observeRuntimeControls(ctx context.Context, runtime bt.ExecutionRuntime, attempt *Attempt) (bt.ExecutionRuntime, func()) {
	noop := func() {}
	if attempt == nil || attempt.ObserveControl == nil {
		return runtime, noop
	}
	input := runtime.Controls()
	if input == nil {
		return runtime, noop
	}
	controls := make(chan bt.ControlRequest)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer close(controls)
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case control, ok := <-input:
				if !ok {
					return
				}
				attempt.ObserveControl(control)
				select {
				case controls <- control:
				case <-ctx.Done():
					return
				case <-stop:
					return
				}
			}
		}
	}()
	var once sync.Once
	return &observedControlRuntime{ExecutionRuntime: runtime, controls: controls}, func() {
		once.Do(func() { close(stop) })
		<-done
	}
}
