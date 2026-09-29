package eino

import (
	"context"
	"errors"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ObserveAgent projects assistant text/reasoning into this attempt's event sink
// without materializing messages or owning native runner/checkpoint state.
// Interrupt callbacks may stage host metadata only: publication must wait for
// JoinAndClose and the host's fenced lifecycle transaction.
func (p *PreparedAttempt) ObserveAgent(inner adk.ResumableAgent, onInterrupt func(context.Context, []ExecutionInterruptBinding) error) adk.ResumableAgent {
	return &preparedObserver{ResumableAgent: inner, attempt: p, onInterrupt: onInterrupt}
}

// ObserveAgentWithNativeCancel accepts a trusted attempt-local observation of
// manager control delivery. Eino's outer Runner converts cancel graph
// interrupts only after this observer sees them. During that native control,
// opaque system roots must reach the Runner unchanged so it can save and
// classify the checkpoint. Real permission targets are still validated and
// staged, including a permission interrupt that wins a race against drain.
// The callback must not infer control from model output or checkpoint payloads.
func (p *PreparedAttempt) ObserveAgentWithNativeCancel(inner adk.ResumableAgent, onInterrupt func(context.Context, []ExecutionInterruptBinding) error, cancelRequested func() bool) adk.ResumableAgent {
	return &preparedObserver{ResumableAgent: inner, attempt: p, onInterrupt: onInterrupt, nativeCancelRequested: cancelRequested}
}

type preparedObserver struct {
	adk.ResumableAgent
	attempt               *PreparedAttempt
	onInterrupt           func(context.Context, []ExecutionInterruptBinding) error
	nativeCancelRequested func() bool
}

func (a *preparedObserver) Run(ctx context.Context, input *adk.AgentInput, options ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.observe(ctx, func(ctx context.Context) *adk.AsyncIterator[*adk.AgentEvent] {
		return a.ResumableAgent.Run(ctx, input, options...)
	})
}

func (a *preparedObserver) Resume(ctx context.Context, info *adk.ResumeInfo, options ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return a.observe(ctx, func(ctx context.Context) *adk.AsyncIterator[*adk.AgentEvent] {
		return a.ResumableAgent.Resume(ctx, info, options...)
	})
}

func (a *preparedObserver) observe(parent context.Context, begin func(context.Context) *adk.AsyncIterator[*adk.AgentEvent]) *adk.AsyncIterator[*adk.AgentEvent] {
	output, writer := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	p := a.attempt
	invocation, cancel := context.WithCancel(parent)
	stopAttempt := context.AfterFunc(p.ctx, cancel)
	ctx, finish, err := p.io.begin(invocation)
	if err != nil {
		stopAttempt()
		cancel()
		writer.Send(&adk.AgentEvent{Err: err})
		writer.Close()
		return output
	}
	if p.ctx.Err() != nil {
		cancel()
	}
	go func() {
		defer writer.Close()
		defer cancel()
		defer stopAttempt()
		defer finish()
		var streams sync.WaitGroup
		var sawEventError bool
		defer func() {
			streams.Wait()
			if !sawEventError {
				if budgetErr := p.BudgetFailure(); budgetErr != nil {
					writer.Send(&adk.AgentEvent{Err: budgetErr})
				}
			}
		}()
		fail := func(err error) {
			p.io.recordError(err)
			cancel()
		}
		source := begin(ctx)
		if source == nil {
			err := errors.New("prepared agent returned no event iterator")
			fail(err)
			writer.Send(&adk.AgentEvent{Err: err})
			return
		}
		for {
			event, ok := source.Next()
			if !ok {
				return
			}
			if event == nil {
				continue
			}
			projected := *event
			if event.Err != nil {
				sawEventError = true
			}
			if event.Action != nil && event.Action.Interrupted != nil {
				bindings, observeErr := a.interruptBindings(event.Action.Interrupted.InterruptContexts)
				if observeErr == nil && len(bindings) > 0 && a.onInterrupt != nil {
					observeErr = a.onInterrupt(ctx, bindings)
				}
				if observeErr != nil {
					fail(observeErr)
					// Do not expose a successful native interrupt after host staging
					// failed; the executor must receive an ordinary failure.
					projected.Action = nil
					projected.Err = errors.Join(projected.Err, observeErr)
					sawEventError = true
				}
			}
			if event.Output != nil && event.Output.MessageOutput != nil {
				out := *event.Output
				variant := *event.Output.MessageOutput
				out.MessageOutput = &variant
				projected.Output = &out
				observe := func(message *schema.Message) error {
					return p.observeText(ctx, variant.Role, message)
				}
				if variant.IsStreaming && variant.MessageStream != nil {
					streams.Add(1)
					var closeMu sync.Mutex
					var stopClose func() bool
					finished := false
					stream := relayStream(variant.MessageStream, func() {
						closeMu.Lock()
						finished = true
						if stopClose != nil {
							stopClose()
						}
						closeMu.Unlock()
						streams.Done()
					}, observe, fail)
					closeMu.Lock()
					if !finished {
						stopClose = context.AfterFunc(p.io.ctx, stream.Close)
					}
					closeMu.Unlock()
					stream.SetAutomaticClose()
					variant.MessageStream = stream
				} else if observeErr := observe(variant.Message); observeErr != nil {
					fail(observeErr)
					projected.Err = errors.Join(projected.Err, observeErr)
					sawEventError = true
				}
			}
			writer.Send(&projected)
		}
	}()
	return output
}

func (a *preparedObserver) interruptBindings(contexts []*adk.InterruptCtx) ([]ExecutionInterruptBinding, error) {
	if a.nativeCancelRequested == nil || !a.nativeCancelRequested() {
		return PermissionInterruptBindings(contexts)
	}
	var permissions []*adk.InterruptCtx
	for _, item := range contexts {
		if item != nil {
			if _, ok := item.Info.(*permissionInterrupt); ok {
				permissions = append(permissions, item)
			}
		}
	}
	if len(permissions) == 0 {
		return nil, nil
	}
	return PermissionInterruptBindings(permissions)
}

func (p *PreparedAttempt) observeText(ctx context.Context, role schema.RoleType, message *schema.Message) error {
	if message == nil || (role != schema.Assistant && (role != "" || message.Role != schema.Assistant)) {
		return nil
	}
	if message.Content != "" {
		if err := p.sink.emit(ctx, harness.RunEvent{Kind: "text_delta", Text: message.Content}); err != nil {
			return err
		}
	}
	if message.ReasoningContent != "" {
		return p.sink.emit(ctx, harness.RunEvent{Kind: "reasoning_delta", Text: message.ReasoningContent})
	}
	return nil
}
