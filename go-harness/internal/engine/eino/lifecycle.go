package eino

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
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

// relayStream owns upstream reads through terminal EOF. A provider may send its
// error before deferred HTTP/process cleanup runs, so merely reading one error
// does not establish teardown. Providers must honor their cancelled context.
func relayStream[T any](source *schema.StreamReader[T], finish func(), onChunk func(T) error) *schema.StreamReader[T] {
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
			if draining {
				continue
			}
			if err != nil {
				writer.Send(chunk, err)
				draining = true
				continue
			}
			if onChunk != nil {
				if callbackErr := onChunk(chunk); callbackErr != nil {
					writer.Send(chunk, callbackErr)
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
	io *runIO
}

func (m *modelLifecycle) WrapModel(_ context.Context, inner model.BaseModel[*schema.Message], _ *adk.ModelContext) (model.BaseModel[*schema.Message], error) {
	return &trackedModel{inner: inner, io: m.io}, nil
}

type trackedModel struct {
	inner model.BaseModel[*schema.Message]
	io    *runIO
}

func (m *trackedModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	ctx, finish, err := m.io.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	return m.inner.Generate(ctx, input, opts...)
}
func (m *trackedModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	ctx, finish, err := m.io.begin(ctx)
	if err != nil {
		return nil, err
	}
	source, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		finish()
		return nil, err
	}
	return relayStream(source, finish, nil), nil
}
