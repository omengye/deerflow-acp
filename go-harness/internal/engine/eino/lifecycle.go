package eino

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// runIO separates checkpoint persistence from cancellable external work. Native
// immediate cancellation can finish its graph before a provider stream exits;
// this tracker joins cooperative model/tool calls before releasing the run.
type runIO struct {
	mu     sync.Mutex
	closed bool
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	err    error
}

func newRunIO(ctx context.Context) *runIO {
	ioCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &runIO{ctx: ioCtx, cancel: cancel}
}

func (r *runIO) begin(ctx context.Context) (context.Context, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, context.Canceled
	}
	r.wg.Add(1)
	workCtx, cancel := context.WithCancel(ctx)
	stopHook := context.AfterFunc(r.ctx, cancel)
	var once sync.Once
	return workCtx, func() { once.Do(func() { stopHook(); cancel(); r.wg.Done() }) }, nil
}

func (r *runIO) stop()         { r.mu.Lock(); r.closed = true; r.cancel(); r.mu.Unlock() }
func (r *runIO) closeAndWait() { r.stop(); r.wg.Wait() }
func (r *runIO) recordError(err error) {
	err = withoutNativeTermination(err)
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = errors.Join(r.err, err)
}
func (r *runIO) failure() error { r.mu.Lock(); defer r.mu.Unlock(); return r.err }

// relayStream owns upstream reads through terminal EOF. A provider may send its
// error before deferred HTTP/process cleanup runs, so merely reading one error
// does not establish teardown. Providers must honor their cancelled context.
func relayStream[T any](source *schema.StreamReader[T], finish func(), onChunk func(T) error, onError func(error), observers ...func(T) error) *schema.StreamReader[T] {
	reader, writer := schema.Pipe[T](1)
	go func() {
		defer finish()
		defer source.Close()
		defer writer.Close()
		draining := false
		for {
			chunk, err := source.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil && onError != nil {
				onError(err)
			}
			if err == nil {
				for _, observe := range observers {
					if observeErr := observe(chunk); observeErr != nil {
						if onError != nil {
							onError(observeErr)
						}
						if !draining {
							var zero T
							writer.Send(zero, observeErr)
							draining = true
						}
					}
				}
			}
			if draining {
				continue
			}
			if err != nil {
				var zero T
				writer.Send(zero, err)
				draining = true
				continue
			}
			if onChunk != nil {
				if callbackErr := onChunk(chunk); callbackErr != nil {
					if onError != nil {
						onError(callbackErr)
					}
					var zero T
					writer.Send(zero, callbackErr)
					draining = true
					continue
				}
			}
			if closed := writer.Send(chunk, nil); closed {
				draining = true
			}
		}
	}()
	return reader
}

type modelLifecycle struct {
	adk.BaseChatModelAgentMiddleware
	io     *runIO
	budget *runBudget
	sink   *eventSink
	media  *mediaProjection
}

func (m *modelLifecycle) WrapModel(_ context.Context, inner model.BaseModel[*schema.Message], _ *adk.ModelContext) (model.BaseModel[*schema.Message], error) {
	return &trackedModel{inner: inner, io: m.io, budget: m.budget, sink: m.sink, media: m.media}, nil
}

type trackedModel struct {
	inner  model.BaseModel[*schema.Message]
	io     *runIO
	budget *runBudget
	sink   *eventSink
	media  *mediaProjection
}

func (m *trackedModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	ctx, finish, err := m.io.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if m.media != nil {
		input, err = m.media.hydrate(ctx, input, opts)
		if err != nil {
			return nil, err
		}
	}
	reservation, opts, err := m.budget.reserve(input, opts)
	if err != nil {
		return nil, err
	}
	defer m.settle(ctx, reservation)
	msg, err := m.inner.Generate(ctx, input, opts...)
	observeErr := reservation.observe(msg)
	if mediaErr := validateProviderOutput(msg); mediaErr != nil {
		return nil, errors.Join(err, observeErr, mediaErr)
	}
	if observeErr != nil {
		return msg, errors.Join(err, observeErr)
	}
	return msg, err
}
func (m *trackedModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	ctx, finish, err := m.io.begin(ctx)
	if err != nil {
		return nil, err
	}
	if m.media != nil {
		input, err = m.media.hydrate(ctx, input, opts)
		if err != nil {
			finish()
			return nil, err
		}
	}
	reservation, opts, err := m.budget.reserve(input, opts)
	if err != nil {
		finish()
		return nil, err
	}
	source, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		m.settle(ctx, reservation)
		finish()
		return nil, err
	}
	return relayStream(source, func() { m.settle(ctx, reservation); finish() }, validateProviderOutput, m.io.recordError, reservation.observe), nil
}

func (m *trackedModel) settle(ctx context.Context, r *modelReservation) {
	u := r.settle()
	_ = m.sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: "usage", Usage: &u})
}
