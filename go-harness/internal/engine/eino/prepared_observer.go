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

type preparedObserver struct {
	adk.ResumableAgent
	attempt     *PreparedAttempt
	onInterrupt func(context.Context, []ExecutionInterruptBinding) error
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
				bindings, observeErr := PermissionInterruptBindings(event.Action.Interrupted.InterruptContexts)
				if observeErr == nil && a.onInterrupt != nil {
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
