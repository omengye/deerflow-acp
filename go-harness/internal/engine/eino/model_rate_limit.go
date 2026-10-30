package eino

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	openaisdk "github.com/meguminnnnnnnnn/go-openai"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxModelRateLimitRetries = 3

func modelRateLimitResponse(err error) bool {
	switch api := err.(type) {
	case *openaiapi.APIError:
		return api.HTTPStatusCode == http.StatusTooManyRequests
	case *openaisdk.RequestError:
		return api.HTTPStatusCode == http.StatusTooManyRequests
	case *anthropic.Error:
		return api.StatusCode == http.StatusTooManyRequests
	}
	return false
}

func classifyModelRateLimitError(err error) error {
	if errors.Is(err, harness.ErrModelRateLimited) {
		return err
	}
	_, found := removeErrorLeaves(err, modelRateLimitResponse)
	if !found {
		return err
	}
	return fmt.Errorf("%w: %w", harness.ErrModelRateLimited, err)
}

// Only the provider boundary can certify that a request was rejected before
// opening a stream. A 429 discovered after streaming starts is classified for
// display, but must never replay partial output or tool calls.
type rejectedModelRequest struct{ cause error }

func (e *rejectedModelRequest) Error() string { return e.cause.Error() }
func (e *rejectedModelRequest) Unwrap() error { return e.cause }

func rejectedRateLimitRequest(err error) error {
	remaining, found := removeErrorLeaves(err, modelRateLimitResponse)
	if !found || remaining != nil {
		return classifyModelRateLimitError(err)
	}
	return &rejectedModelRequest{cause: classifyModelRateLimitError(err)}
}

func retryableModelRateLimit(err error) bool {
	remaining, found := removeErrorLeaves(err, func(leaf error) bool {
		_, rejected := leaf.(*rejectedModelRequest)
		return rejected
	})
	return found && remaining == nil
}

// Retry the same model boundary, never the agent run or its tools. Each call
// goes through a fresh tracked reservation and settles before the next one.
// Auxiliary models retain their existing caller-owned failure/retry policies.
func retryRejectedModel[T any](ctx context.Context, m *trackedModel, call func(context.Context) (T, error)) (T, error) {
	if !m.nativeEndpoint {
		return call(ctx)
	}
	for attempt := 0; ; attempt++ {
		result, err := call(ctx)
		if attempt == maxModelRateLimitRetries || !retryableModelRateLimit(err) {
			return result, err
		}
		if failure := m.io.failure(); failure != nil {
			return result, errors.Join(err, failure)
		}
		// Waiting must join cancellation without occupying a role's model/tool
		// concurrency slot; the next actual request acquires its own slot.
		work, finish, waitErr := m.io.begin(ctx)
		if waitErr == nil {
			waitErr = waitModelRateLimit(work, time.Second<<attempt)
			finish()
		}
		if waitErr != nil {
			var zero T
			return zero, waitErr
		}
	}
}

func waitModelRateLimit(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
