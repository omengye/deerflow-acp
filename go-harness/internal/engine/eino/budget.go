package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type budgetError struct{ resource string }

func (e *budgetError) Error() string { return "run " + e.resource + " budget exhausted" }
func (e *budgetError) stopReason() string {
	if e.resource == "tokens" {
		return "max_tokens"
	}
	if e.resource == "time" {
		return "cancelled"
	}
	return "max_turn_requests"
}

type runBudget struct {
	mu                    sync.Mutex
	limits                harness.BudgetLimits
	modelCalls, toolCalls int
	spent, held           int64
	exhausted             *budgetError
}

func validateBudget(l harness.BudgetLimits) error {
	if l.MaxModelCalls < 0 || l.MaxToolCalls < 0 || l.MaxTokens < 0 || l.MaxOutputTokens < 0 || l.Timeout < 0 {
		return fmt.Errorf("budget limits must not be negative")
	}
	return nil
}
func (b *runBudget) fail(resource string) *budgetError {
	if b.exhausted == nil {
		b.exhausted = &budgetError{resource}
	}
	return b.exhausted
}
func (b *runBudget) failure() *budgetError { b.mu.Lock(); defer b.mu.Unlock(); return b.exhausted }
func (b *runBudget) tool() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted != nil {
		return b.exhausted
	}
	if b.limits.MaxToolCalls > 0 && b.toolCalls >= b.limits.MaxToolCalls {
		return b.fail("tool_calls")
	}
	b.toolCalls++
	return nil
}

type modelReservation struct {
	budget                       *runBudget
	input, outputBytes, reserved int64
	usage                        harness.Usage
	known                        bool
	once                         sync.Once
}

func (b *runBudget) reserve(input []*schema.Message, opts []model.Option) (*modelReservation, []model.Option, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, nil, err
	}
	common := model.GetCommonOptions(&model.Options{}, opts...)
	toolBytes, err := json.Marshal(common.Tools)
	if err != nil {
		return nil, nil, err
	}
	// A byte estimate, not model-specific tokenization. Provider usage replaces
	// it at settlement; providers can exceed this estimate on the in-flight call.
	estimate := int64((len(encoded)+len(toolBytes)+3)/4 + len(input)*8)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted != nil {
		return nil, nil, b.exhausted
	}
	if b.limits.MaxModelCalls > 0 && b.modelCalls >= b.limits.MaxModelCalls {
		return nil, nil, b.fail("model_calls")
	}
	output := b.limits.MaxOutputTokens
	if common.MaxTokens != nil && *common.MaxTokens > 0 && (output == 0 || *common.MaxTokens < output) {
		output = *common.MaxTokens
	}
	if b.limits.MaxTokens > 0 {
		remaining := b.limits.MaxTokens - b.spent - b.held - estimate
		if remaining <= 0 {
			return nil, nil, b.fail("tokens")
		}
		if output == 0 {
			output = 4096
		}
		if int64(output) > remaining {
			output = int(remaining)
		}
	}
	b.modelCalls++
	reservation := &modelReservation{budget: b, input: estimate, reserved: estimate + int64(output)}
	b.held += reservation.reserved
	if output > 0 {
		opts = append(append([]model.Option(nil), opts...), model.WithMaxTokens(output))
	}
	return reservation, opts, nil
}

// withoutBudgetTermination removes only expected budget/cancellation leaves.
// An errors.Join may also contain a provider or cleanup failure which must
// remain visible even when one sibling denotes normal quota termination.
func withoutBudgetTermination(err error) error {
	remaining, _ := removeErrorLeaves(err, func(leaf error) bool {
		_, quota := leaf.(*budgetError)
		return quota || leaf == context.Canceled || leaf == context.DeadlineExceeded
	})
	return remaining
}

func removeErrorLeaves(err error, remove func(error) bool) (error, bool) {
	if err == nil {
		return nil, false
	}
	if remove(err) {
		return nil, true
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		changed := false
		for _, child := range multi.Unwrap() {
			keep, removed := removeErrorLeaves(child, remove)
			changed = changed || removed
			if keep != nil {
				remaining = append(remaining, keep)
			}
		}
		if !changed {
			return err, false
		}
		return errors.Join(remaining...), true
	}
	if child := errors.Unwrap(err); child != nil {
		keep, changed := removeErrorLeaves(child, remove)
		if !changed {
			return err, false
		}
		if keep == nil {
			return nil, true
		}
		return fmt.Errorf("execution failed: %w", keep), true
	}
	return err, false
}

func (r *modelReservation) observe(msg *schema.Message) error {
	if msg == nil {
		return nil
	}
	r.outputBytes += int64(len(msg.Content) + len(msg.ReasoningContent))
	for _, c := range msg.ToolCalls {
		r.outputBytes += int64(len(c.Function.Arguments) + len(c.Function.Name))
	}
	if meta := msg.ResponseMeta; meta != nil && meta.Usage != nil {
		u := meta.Usage
		if u.TotalTokens > 0 || u.PromptTokens > 0 || u.CompletionTokens > 0 {
			r.known = true
		}
		r.usage.InputTokens = max(r.usage.InputTokens, int64(u.PromptTokens))
		r.usage.OutputTokens = max(r.usage.OutputTokens, int64(u.CompletionTokens))
		r.usage.TotalTokens = max(r.usage.TotalTokens, int64(u.TotalTokens))
	}
	actual := r.currentUsage().TotalTokens
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits.MaxTokens > 0 && b.spent+b.held-r.reserved+actual > b.limits.MaxTokens {
		return b.fail("tokens")
	}
	return nil
}
func (r *modelReservation) currentUsage() harness.Usage {
	u := r.usage
	if !r.known {
		u = harness.Usage{InputTokens: r.input, OutputTokens: (r.outputBytes + 3) / 4, Estimated: true}
	}
	u.TotalTokens = max(u.TotalTokens, u.InputTokens+u.OutputTokens)
	return u
}
func (r *modelReservation) settle() harness.Usage {
	u := r.currentUsage()
	r.once.Do(func() {
		b := r.budget
		b.mu.Lock()
		defer b.mu.Unlock()
		b.held -= r.reserved
		b.spent += u.TotalTokens
		if b.limits.MaxTokens > 0 && b.spent > b.limits.MaxTokens {
			b.fail("tokens")
		}
	})
	return u
}
