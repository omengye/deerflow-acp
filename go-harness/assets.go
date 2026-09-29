package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ListArtifacts returns the session's committed immutable output references.
// The session must be attached to this SDK client and currently idle.
func (c *Client) ListArtifacts(ctx context.Context, sessionID string) ([]harness.Content, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.service.ListArtifacts(ctx, c.owner, sessionID)
}

// ResolveAsset verifies ownership and the stored content hash before returning
// snapshot bytes. Embedded session IDs never authorize cross-session access.
func (c *Client) ResolveAsset(ctx context.Context, sessionID string, ref harness.AssetRef) ([]byte, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.service.ResolveAsset(ctx, c.owner, sessionID, ref)
}
