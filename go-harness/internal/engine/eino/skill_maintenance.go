package eino

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

// Keep host admission causes machine-readable without echoing provider or
// database diagnostics (which may contain credentials or private input).
func skillMaintenanceFailure(err error) error {
	for _, cause := range []error{skillmaintenance.ErrBudget, skillmaintenance.ErrConflict, skillmaintenance.ErrClock, skillmaintenance.ErrInvalid, harness.ErrSkillConflict, harness.ErrPermissionDenied} {
		if errors.Is(err, cause) {
			return fmt.Errorf("%w: %w", harness.ErrSkillValidation, cause)
		}
	}
	return skillReviewError("maintenance model request or accounting failed")
}

func validSkillModelUsage(u *schema.TokenUsage) bool {
	// These stages always send nonempty input and expect nonempty structured
	// output. A zero field may mean that the provider omitted that counter.
	return u != nil && u.PromptTokens > 0 && u.CompletionTokens > 0 && int64(u.PromptTokens) <= 1<<40 && int64(u.CompletionTokens) <= 1<<40 &&
		(u.TotalTokens == 0 || int64(u.TotalTokens) == int64(u.PromptTokens)+int64(u.CompletionTokens))
}

// All isolated maintenance model stages use this one accounting boundary.
// Reservations survive crashes; failed/missing usage retains the complete cap.
func generateSkillMaintenance(ctx context.Context, broker skillmaintenance.Broker, chat model.BaseChatModel, route harness.ModelRoute, stage string, messages []*schema.Message, output int, options ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(messages)
	if err != nil {
		return nil, skillReviewError("maintenance input cannot be encoded")
	}
	identity, _ := json.Marshal([]any{route.Provider, route.Model, route.BaseURL, output})
	reservation, err := broker.Reserve(ctx, skillmaintenance.ModelCall{Stage: stage, ModelIdentity: fmt.Sprintf("%x", sha256.Sum256(identity)), InputDigest: fmt.Sprintf("%x", sha256.Sum256(payload)), InputTokens: int64(len(payload)) + 128, MaxOutputTokens: int64(output)})
	if err != nil {
		return nil, err
	}
	// Options have already been tuned to the same cap admitted above.
	answer, callErr := chat.Generate(ctx, messages, options...)
	usage := skillmaintenance.Usage{}
	if callErr == nil && ctx.Err() == nil && answer != nil && answer.ResponseMeta != nil && answer.ResponseMeta.Usage != nil {
		u := answer.ResponseMeta.Usage
		if validSkillModelUsage(u) {
			usage = skillmaintenance.Usage{InputTokens: int64(u.PromptTokens), OutputTokens: int64(u.CompletionTokens), Known: true}
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := broker.Settle(cleanup, reservation, usage); err != nil {
		return nil, err
	}
	if callErr != nil || ctx.Err() != nil {
		return nil, skillReviewError("maintenance model request failed or timed out")
	}
	return answer, nil
}
