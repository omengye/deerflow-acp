package eino

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// runIO separates checkpoint persistence from cancellable external work. Native
// immediate cancellation can finish its graph before a provider stream exits;
// this tracker joins cooperative model/tool calls before releasing the run.
type runIO struct {
	mu        sync.Mutex
	closed    bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	err       error
	effectErr error
	role      bool
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

func (r *runIO) beginEffect(ctx context.Context) (context.Context, func(), error) {
	tracked, finish, err := r.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	release, err := beginRoleWork(tracked)
	if err != nil {
		finish()
		return nil, nil, err
	}
	return tracked, func() { release(); finish() }, nil
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
func (r *runIO) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.err, r.effectErr)
}

// Cancellation of a started tool is not evidence that the tool had no effect.
// Keep this separately from native cancellation filtering, including late
// results observed while joining the role after its iterator has ended.
func (r *runIO) recordUnconfirmedEffect(err error) {
	if !r.role || err == nil || certifiedToolOutcome(err) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.effectErr = errors.Join(r.effectErr, &roleEffectFailure{cause: err})
}

// relayStream owns upstream reads through terminal EOF. A provider may send its
// error before deferred HTTP/process cleanup runs, so merely reading one error
// does not establish teardown. Providers must honor their cancelled context.
func relayStream[T any](source *schema.StreamReader[T], finish func(), onChunk func(T) (T, error), onError func(error), observers ...func(T) error) *schema.StreamReader[T] {
	return relayMappedStream(source, finish, onChunk, onError, nil, observers...)
}

func relayMappedStream[T any](source *schema.StreamReader[T], finish func(), onChunk func(T) (T, error), onError func(error), sourceError func(error) error, observers ...func(T) error) *schema.StreamReader[T] {
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
			if err != nil && sourceError != nil {
				err = sourceError(err)
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
				mapped, callbackErr := onChunk(chunk)
				if callbackErr != nil {
					if onError != nil {
						onError(callbackErr)
					}
					var zero T
					writer.Send(zero, callbackErr)
					draining = true
					continue
				}
				chunk = mapped
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
	io       *runIO
	budget   *runBudget
	sink     *eventSink
	media    *mediaProjection
	settings harness.AgentModelSettings
	tuning   *roleModelTuning
	role     bool
}

func (m *modelLifecycle) WrapModel(_ context.Context, inner model.BaseModel[*schema.Message], _ *adk.ModelContext) (model.BaseModel[*schema.Message], error) {
	return &trackedModel{inner: inner, io: m.io, budget: m.budget, sink: m.sink, media: m.media, settings: m.settings, tuning: m.tuning, role: m.role}, nil
}

type trackedModel struct {
	inner    model.BaseModel[*schema.Message]
	io       *runIO
	budget   *runBudget
	sink     *eventSink
	media    *mediaProjection
	settings harness.AgentModelSettings
	tuning   *roleModelTuning
	role     bool
}

func (m *trackedModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	opts = roleModelOptions(m.settings, opts)
	opts = m.tuning.beforeReservation(opts)
	ctx, finish, err := m.io.beginEffect(ctx)
	if err != nil {
		m.recordAdmissionFailure(err)
		return nil, err
	}
	defer finish()
	if m.media != nil {
		input, err = m.media.hydrate(ctx, input, opts)
		if err != nil {
			return nil, err
		}
	}
	reservation, opts, err := m.budget.reserveContext(ctx, input, opts)
	if err != nil {
		m.recordAdmissionFailure(err)
		return nil, err
	}
	complete := false
	defer func() { m.settle(ctx, reservation, complete && ctx.Err() == nil) }()
	opts, err = m.providerOptions(reservation, opts)
	if err != nil {
		complete = true // No provider I/O: settle the admission with zero tokens.
		return nil, err
	}
	endModel := beginRoleActivity(ctx, "model")
	msg, err := m.inner.Generate(ctx, input, opts...)
	endModel()
	observeErr := reservation.observe(msg)
	providerErr := m.modelError(err)
	err = nil
	if m.media != nil {
		msg, err = m.media.normalizeProviderOutput(ctx, msg, &modelImageLimit{})
	} else if msg != nil {
		err = validateProviderOutput(msg, "")
	}
	complete = err == nil && providerErr == nil
	if err != nil || providerErr != nil {
		return nil, errors.Join(providerErr, err, observeErr)
	}
	if observeErr != nil {
		return msg, observeErr
	}
	return msg, nil
}
func (m *trackedModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	opts = roleModelOptions(m.settings, opts)
	opts = m.tuning.beforeReservation(opts)
	ctx, finish, err := m.io.beginEffect(ctx)
	if err != nil {
		m.recordAdmissionFailure(err)
		return nil, err
	}
	if m.media != nil {
		input, err = m.media.hydrate(ctx, input, opts)
		if err != nil {
			finish()
			return nil, err
		}
	}
	reservation, opts, err := m.budget.reserveContext(ctx, input, opts)
	if err != nil {
		m.recordAdmissionFailure(err)
		finish()
		return nil, err
	}
	opts, err = m.providerOptions(reservation, opts)
	if err != nil {
		m.settle(ctx, reservation, true)
		finish()
		return nil, err
	}
	endModel := beginRoleActivity(ctx, "model")
	source, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		endModel()
		m.settle(ctx, reservation, false)
		finish()
		return nil, m.modelError(err)
	}
	if source == nil {
		endModel()
		m.settle(ctx, reservation, false)
		finish()
		return nil, m.modelError(errors.New("model returned nil stream"))
	}
	complete := true
	limit := &modelImageLimit{}
	project := func(msg *schema.Message) (*schema.Message, error) {
		if m.media != nil {
			return m.media.normalizeProviderOutput(ctx, msg, limit)
		}
		return msg, validateProviderOutput(msg, "")
	}
	return relayMappedStream(source, func() { endModel(); m.settle(ctx, reservation, complete && ctx.Err() == nil); finish() }, project, func(err error) { complete = false; m.io.recordError(err) }, m.modelError, reservation.observe), nil
}

// Nested consumers such as skill review intentionally sanitize provider errors.
// Admission/storage failures must also reach the owning run, even if that
// consumer returns a structured failure and the next model call succeeds.
func (m *trackedModel) recordAdmissionFailure(err error) {
	remaining, _ := removeErrorLeaves(err, func(leaf error) bool {
		_, quota := leaf.(*budgetError)
		return quota || leaf == context.Canceled || leaf == context.DeadlineExceeded
	})
	m.io.recordError(remaining)
}

func (m *trackedModel) providerOptions(reservation *modelReservation, opts []model.Option) ([]model.Option, error) {
	result, err := m.tuning.afterReservation(opts)
	if err != nil {
		// Reservation succeeded but no provider call has begun. Keep the model
		// admission count and release every held token, recording zero usage.
		reservation.known = true
		m.budget.mu.Lock()
		m.budget.fail("tokens")
		m.budget.mu.Unlock()
	}
	return result, err
}

func (m *trackedModel) settle(ctx context.Context, r *modelReservation, complete bool) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	u, err := r.settleContext(cleanup, complete)
	if err != nil {
		m.io.recordError(err)
		return
	}
	if err = m.sink.emit(cleanup, harness.RunEvent{Kind: "usage", Usage: &u}); err != nil {
		m.io.recordError(err)
	}
}
