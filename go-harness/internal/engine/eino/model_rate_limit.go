package eino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	openaisdk "github.com/meguminnnnnnnnn/go-openai"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxModelRateLimitRetries = 3

// Transient provider failures (5xx, overload, dropped connections) are retried
// more patiently than 429s so a multi-hour task survives a short outage:
// 2s, 4s, 8s, 16s, 32s, 60s (about two minutes in total).
const maxModelTransientRetries = 6
const maxModelTransientBackoff = 60 * time.Second

func transientStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// modelTransientResponse classifies one error node. Cancellation and caller
// deadlines are never transient, even when wrapped by a network error.
func modelTransientResponse(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch api := err.(type) {
	case *openaiapi.APIError:
		return transientStatus(api.HTTPStatusCode)
	case *openaisdk.RequestError:
		return transientStatus(api.HTTPStatusCode)
	case *anthropic.Error:
		return transientStatus(api.StatusCode)
	case *url.Error, *net.OpError:
		return true
	}
	return err == io.ErrUnexpectedEOF || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}

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
type rejectedModelRequest struct {
	cause     error
	transient bool
}

func (e *rejectedModelRequest) Error() string { return e.cause.Error() }
func (e *rejectedModelRequest) Unwrap() error { return e.cause }

func rejectedRateLimitRequest(err error) error {
	if remaining, found := removeErrorLeaves(err, modelTransientResponse); found && remaining == nil {
		return &rejectedModelRequest{cause: err, transient: true}
	}
	remaining, found := removeErrorLeaves(err, modelRateLimitResponse)
	if !found || remaining != nil {
		return classifyModelRateLimitError(err)
	}
	return &rejectedModelRequest{cause: classifyModelRateLimitError(err)}
}

func retryableModelRateLimit(err error) bool {
	_, ok := retryableRejection(err)
	return ok
}

// retryableRejection reports whether every failure leaf was certified as
// rejected before a stream opened, and whether any of them was transient.
func retryableRejection(err error) (transient, ok bool) {
	remaining, found := removeErrorLeaves(err, func(leaf error) bool {
		rejected, is := leaf.(*rejectedModelRequest)
		if is && rejected.transient {
			transient = true
		}
		return is
	})
	return transient, found && remaining == nil
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
		transient, retryable := retryableRejection(err)
		limit, delay := maxModelRateLimitRetries, time.Second<<attempt
		if transient {
			limit, delay = maxModelTransientRetries, min(2*time.Second<<attempt, maxModelTransientBackoff)
		}
		if attempt >= limit || !retryable || ctx.Err() != nil {
			return result, err
		}
		if failure := m.io.failure(); failure != nil {
			return result, errors.Join(err, failure)
		}
		// Waiting must join cancellation without occupying a role's model/tool
		// concurrency slot; the next actual request acquires its own slot.
		work, finish, waitErr := m.io.begin(ctx)
		if waitErr == nil {
			waitErr = waitModelRateLimit(work, delay)
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
