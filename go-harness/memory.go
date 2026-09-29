package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ImportDeerMem imports one explicitly selected legacy memory.json into an
// attached scope. The caller chooses the destination; hashed Python buckets
// are never reverse-mapped to a workspace or user implicitly.
func (c *Client) ImportDeerMem(ctx context.Context, sessionID string, kind harness.MemoryScope, raw []byte) (harness.LegacyImportReport, error) {
	done, err := c.operation()
	if err != nil {
		return harness.LegacyImportReport{}, err
	}
	defer done()
	return c.service.ImportDeerMem(ctx, c.owner, sessionID, kind, raw)
}

// FlushMemory waits until the session's earlier synchronous memory writes are
// visible. It uses ctx as the wait bound and does not rerun skipped extraction.
func (c *Client) FlushMemory(ctx context.Context, sessionID string) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.service.FlushMemory(ctx, c.owner, sessionID)
}

// MemoryFacts lists descriptive facts in one attached scope. Memory operations
// hold the same foreground slot as prompts, preserving a run's selected view.
func (c *Client) MemoryFacts(ctx context.Context, sessionID string, kind harness.MemoryScope, after string, limit int) (harness.MemoryPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.MemoryPage{}, err
	}
	defer done()
	return c.service.MemoryFacts(ctx, c.owner, sessionID, kind, after, limit)
}

func (c *Client) SearchMemory(ctx context.Context, sessionID string, kind harness.MemoryScope, query string, limit int) ([]harness.MemoryFact, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.service.SearchMemory(ctx, c.owner, sessionID, kind, query, limit)
}

func (c *Client) MemoryFact(ctx context.Context, sessionID string, kind harness.MemoryScope, id string) (harness.MemoryFact, error) {
	done, err := c.operation()
	if err != nil {
		return harness.MemoryFact{}, err
	}
	defer done()
	return c.service.MemoryFact(ctx, c.owner, sessionID, kind, id)
}

func (c *Client) CreateMemoryFact(ctx context.Context, sessionID string, kind harness.MemoryScope, candidate harness.MemoryCandidate) (harness.MemoryFact, error) {
	done, err := c.operation()
	if err != nil {
		return harness.MemoryFact{}, err
	}
	defer done()
	return c.service.CreateMemoryFact(ctx, c.owner, sessionID, kind, candidate)
}

func (c *Client) ReplaceMemoryFact(ctx context.Context, sessionID string, kind harness.MemoryScope, id string, expectedRevision int64, candidate harness.MemoryCandidate) (harness.MemoryFact, error) {
	done, err := c.operation()
	if err != nil {
		return harness.MemoryFact{}, err
	}
	defer done()
	return c.service.ReplaceMemoryFact(ctx, c.owner, sessionID, kind, id, expectedRevision, candidate)
}

func (c *Client) DeleteMemoryFact(ctx context.Context, sessionID string, kind harness.MemoryScope, id string, expectedRevision int64) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.service.DeleteMemoryFact(ctx, c.owner, sessionID, kind, id, expectedRevision)
}

func (c *Client) ClearMemory(ctx context.Context, sessionID string, kind harness.MemoryScope, expectedScopeRevision int64) (int, error) {
	done, err := c.operation()
	if err != nil {
		return 0, err
	}
	defer done()
	return c.service.ClearMemory(ctx, c.owner, sessionID, kind, expectedScopeRevision)
}
