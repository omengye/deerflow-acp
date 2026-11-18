package deerflow

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"log/slog"
	"time"
)

func (c *Client) startMemoryIndexWorker() {
	c.memoryWorkerMu.Lock()
	defer c.memoryWorkerMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return
	}
	if c.memoryIndexDone != nil {
		select {
		case <-c.memoryIndexDone:
		default:
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.memoryIndexCancel, c.memoryIndexDone = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			call, stop := context.WithTimeout(ctx, 15*time.Second)
			err := c.service.Memory.IndexPending(call)
			stop()
			if err != nil && ctx.Err() == nil {
				slog.Warn("memory index batch deferred")
			}
		}
	}()
}
func (c *Client) stopMemoryWorkers() {
	c.memoryWorkerMu.Lock()
	if c.memoryCancel != nil {
		c.memoryCancel()
	}
	if c.memoryIndexCancel != nil {
		c.memoryIndexCancel()
	}
	extract, index := c.memoryDone, c.memoryIndexDone
	c.memoryWorkerMu.Unlock()
	if extract != nil {
		<-extract
	}
	if index != nil {
		<-index
	}
}

// MemoryIndexFailures exposes current revision/model retries to local operators.
func (c *Client) MemoryIndexFailures(ctx context.Context, limit int) ([]harness.MemoryIndexFailure, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.service.Memory.IndexFailures(ctx, limit)
}
