package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func (c *Client) startMemoryWorker(ctx context.Context, cfg Config) error {
	c.memoryWorkerMu.Lock()
	defer c.memoryWorkerMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return harness.ErrInvalidInput
	}
	if c.memoryDone != nil {
		select {
		case <-c.memoryDone:
		default:
			return nil
		}
	}
	extract, err := einoengine.NewMemoryExtractor(ctx, einoengine.Config{API: cfg.API, Network: cfg.Network, Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model, OutputTokenParameter: cfg.OutputTokenParameter})
	if err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	c.memoryCancel, c.memoryDone = cancel, make(chan struct{})
	done := c.memoryDone
	c.service.Memory.SetExtractionWorkerRunning(true)
	go func() {
		defer close(done)
		defer c.service.Memory.SetExtractionWorkerRunning(false)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			}
			j, err := c.service.Memory.ClaimJob(workerCtx)
			if errors.Is(err, harness.ErrNotFound) {
				continue
			}
			if err != nil {
				slog.Warn("memory worker admission unavailable")
				continue
			}
			if err = c.processMemoryJob(workerCtx, extract, j); err != nil && workerCtx.Err() == nil {
				slog.Warn("memory job requires retry or reconciliation", "job_id", j.ID)
			}
		}
	}()
	return nil
}

// One invocation owns exactly one claim and at most one HTTP dispatch. Retries
// are persisted queue transitions and always receive a new budget reservation.
func (c *Client) processMemoryJob(ctx context.Context, extract *einoengine.MemoryExtractor, j memory.Job) (err error) {
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	done := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		interval := min(5*time.Second, c.budgets.HeartbeatInterval())
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				probe, stop := context.WithTimeout(context.Background(), 5*time.Second)
				e := c.service.Memory.HeartbeatJob(probe, j)
				if e == nil {
					e = c.budgets.TaskTreeRootStopped(probe, j.RootBudgetID)
				}
				stop()
				if e != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		close(done)
		<-joined
		if err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			// A failed cleanup leaves the running lease for durable recovery.
			_ = c.service.Memory.AbortJob(cleanup, j, err)
		}
	}()
	result := j.Result
	if result == "" {
		payload, e := json.Marshal(map[string]string{"user": j.Text})
		if e != nil {
			return e
		}
		req, e := extract.Prepare(memory.ExtractionInstruction, string(payload))
		if e != nil {
			return e
		}
		grant, e := c.service.Memory.ReserveJobCall(callCtx, j, req.Digest, req.InputTokens, 1024)
		if e != nil {
			return e
		}
		first, e := c.service.Memory.DispatchJobCall(callCtx, j, grant)
		if e != nil {
			return e
		}
		if !first {
			return budget.ErrUnknown
		}
		response := extract.Stream(callCtx, req, grant.MaxOutputTokens)
		// Join provider I/O before closing the attempt, even if Close cancelled ctx.
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if e = c.service.Memory.SaveJobResponse(cleanup, j, response); e != nil {
			return e
		}
		if e = c.service.Memory.SettleJobResponse(cleanup, j); e != nil {
			return e
		}
		if !response.Complete {
			return budget.ErrUnknown
		}
		if response.ErrorCode != "" {
			return nil
		}
		if _, e = memory.ParseAttributes(response.Raw, j.Text); e != nil {
			return nil
		}
		result = response.Raw
	}
	return c.service.Memory.ApplyJob(callCtx, j, result)
}
