package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
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
	startedAt             time.Time
	elapsed               time.Duration
	ledger                *durablebudget.Ledger
	scope                 durablebudget.Scope
}

// budgetSnapshot is taken only after all model/tool I/O has joined, so token
// reservations have settled. Paused time is excluded from the execution limit.
type budgetSnapshot struct {
	ModelCalls int           `json:"modelCalls"`
	ToolCalls  int           `json:"toolCalls"`
	Tokens     int64         `json:"tokens"`
	Elapsed    time.Duration `json:"elapsed"`
	Exhausted  string        `json:"exhausted,omitempty"`
}

func (b *runBudget) snapshot() (budgetSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held != 0 {
		return budgetSnapshot{}, errors.New("checkpoint budget has unsettled model reservations")
	}
	s := budgetSnapshot{ModelCalls: b.modelCalls, ToolCalls: b.toolCalls, Tokens: b.spent, Elapsed: b.elapsed}
	if !b.startedAt.IsZero() {
		s.Elapsed += time.Since(b.startedAt)
	}
	if b.exhausted != nil {
		s.Exhausted = b.exhausted.resource
	}
	return s, nil
}

func (b *runBudget) restore(s budgetSnapshot) error {
	if s.ModelCalls < 0 || s.ToolCalls < 0 || s.Tokens < 0 || s.Elapsed < 0 {
		return fmt.Errorf("%w: invalid checkpoint budget", harness.ErrInvalidInput)
	}
	switch s.Exhausted {
	case "", "model_calls", "tool_calls", "tokens", "time":
	default:
		return fmt.Errorf("%w: unknown checkpoint budget resource", harness.ErrInvalidInput)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.modelCalls, b.toolCalls, b.spent, b.elapsed = s.ModelCalls, s.ToolCalls, s.Tokens, s.Elapsed
	if s.Exhausted != "" {
		b.exhausted = &budgetError{resource: s.Exhausted}
	}
	return nil
}

func (b *runBudget) remainingTime() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limits.Timeout - b.elapsed
	if !b.startedAt.IsZero() {
		remaining -= time.Since(b.startedAt)
	}
	return remaining
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

// Optional model work must leave an already completed answer intact when the
// shared root cannot afford another bounded call. The subsequent reservation
// remains authoritative; this read is only an early skip.
func (b *runBudget) canOptionalModelCall(ctx context.Context, minimumTokens int64) bool {
	if b.limits.Timeout > 0 && b.remainingTime() < 5*time.Second {
		return false
	}
	if b.ledger != nil {
		snapshot, err := b.ledger.Snapshot(ctx, b.scope.RootBudgetID)
		if err != nil || snapshot.BlockedReason != "" {
			return false
		}
		if snapshot.Limits.MaxModelCalls > 0 && snapshot.ModelCalls >= snapshot.Limits.MaxModelCalls {
			return false
		}
		return snapshot.Limits.MaxTokens == 0 || snapshot.Limits.MaxTokens-snapshot.SpentTokens-snapshot.HeldTokens >= minimumTokens
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted != nil || b.limits.MaxModelCalls > 0 && b.modelCalls >= b.limits.MaxModelCalls {
		return false
	}
	return b.limits.MaxTokens == 0 || b.limits.MaxTokens-b.spent-b.held >= minimumTokens
}
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
	budget                                     *runBudget
	input, outputBytes, outputImages, reserved int64
	usage                                      harness.Usage
	known                                      bool
	once                                       sync.Once
	grant                                      *durablebudget.Reservation
	settledUsage                               harness.Usage
	settlementErr                              error
}

func (b *runBudget) reserve(input []*schema.Message, opts []model.Option) (*modelReservation, []model.Option, error) {
	return b.reserveContext(context.Background(), input, opts)
}

func (b *runBudget) reserveContext(ctx context.Context, input []*schema.Message, opts []model.Option) (*modelReservation, []model.Option, error) {
	encoded, imageTokens, err := budgetMessageEncoding(input)
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
	estimate := int64((len(encoded)+len(toolBytes)+3)/4+len(input)*8) + imageTokens
	if b.ledger != nil {
		return b.reserveDurable(ctx, estimate, encoded, toolBytes, common, opts)
	}
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

// Each image reserves a documented 4,096-token estimate, independent of its
// transport's Base64 byte count. This is a model-agnostic estimate, not a price
// or hard token bound; actual provider usage replaces it during settlement.
const estimatedImageTokens int64 = 4096

func budgetMessageEncoding(input []*schema.Message) ([]byte, int64, error) {
	messages := make([]*schema.Message, len(input))
	var imageTokens int64
	for i, original := range input {
		if original == nil {
			continue
		}
		message := *original
		message.UserInputMultiContent = append([]schema.MessageInputPart(nil), original.UserInputMultiContent...)
		for j, part := range message.UserInputMultiContent {
			if part.Type == schema.ChatMessagePartTypeImageURL {
				imageTokens += estimatedImageTokens
				part.Image, part.Extra = nil, nil
				message.UserInputMultiContent[j] = part
			}
		}
		message.MultiContent = append([]schema.ChatMessagePart(nil), original.MultiContent...)
		for j, part := range message.MultiContent {
			if part.Type == schema.ChatMessagePartTypeImageURL {
				imageTokens += estimatedImageTokens
				part.ImageURL = nil
				message.MultiContent[j] = part
			}
		}
		message.AssistantGenMultiContent = append([]schema.MessageOutputPart(nil), original.AssistantGenMultiContent...)
		for j, part := range message.AssistantGenMultiContent {
			if part.Type == schema.ChatMessagePartTypeImageURL {
				imageTokens += estimatedImageTokens
				part.Image, part.Extra = nil, nil
				message.AssistantGenMultiContent[j] = part
			}
		}
		messages[i] = &message
	}
	data, err := json.Marshal(messages)
	return data, imageTokens, err
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
	if atomic, ok := err.(interface{ PersistenceFailure() bool }); ok && atomic.PersistenceFailure() {
		return err, false
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
	for _, part := range msg.AssistantGenMultiContent {
		if msg.Content == "" {
			r.outputBytes += int64(len(part.Text))
		}
		if part.Type == schema.ChatMessagePartTypeImageURL {
			r.outputImages++
		}
	}
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
	if b.ledger != nil {
		// Final settlement is authoritative across concurrent attempts. Stop a
		// single response that already exceeds the complete root token policy.
		if b.limits.MaxTokens > 0 && actual > b.limits.MaxTokens {
			b.mu.Lock()
			defer b.mu.Unlock()
			return b.fail("tokens")
		}
		return nil
	}
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
		u = harness.Usage{InputTokens: r.input, OutputTokens: (r.outputBytes+3)/4 + r.outputImages*estimatedImageTokens, Estimated: true}
	}
	u.TotalTokens = max(u.TotalTokens, u.InputTokens+u.OutputTokens)
	return u
}
func (r *modelReservation) settle() harness.Usage {
	u, _ := r.settleContext(context.Background(), true)
	return u
}

func (r *modelReservation) settleContext(ctx context.Context, complete bool) (harness.Usage, error) {
	u := r.currentUsage()
	r.once.Do(func() {
		b := r.budget
		if b.ledger != nil {
			r.settledUsage, r.settlementErr = b.ledger.Settle(ctx, *r.grant, durablebudget.Settlement{Usage: u, Complete: complete})
			if r.settlementErr == nil {
				var state durablebudget.Snapshot
				state, r.settlementErr = b.ledger.Snapshot(ctx, b.scope.RootBudgetID)
				if r.settlementErr == nil && (state.BlockedReason == "tokens" || state.BlockedReason == "time") {
					b.mu.Lock()
					b.fail(state.BlockedReason)
					b.mu.Unlock()
				}
			}
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.held -= r.reserved
		b.spent += u.TotalTokens
		if b.limits.MaxTokens > 0 && b.spent > b.limits.MaxTokens {
			b.fail("tokens")
		}
		r.settledUsage = u
	})
	return r.settledUsage, r.settlementErr
}
