package deerflow

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (h *backgroundHost) ReadTaskArtifact(ctx context.Context, actor harness.TaskActor, request harness.TaskArtifactReadRequest) (harness.TaskArtifactChunk, error) {
	return h.runtime.ReadTaskArtifact(ctx, actor, request)
}

var _ harness.TaskArtifactReader = (*backgroundHost)(nil)
var _ harness.TaskArtifactLister = (*backgroundHost)(nil)

func (h *backgroundHost) TaskArtifacts(ctx context.Context, actor harness.TaskActor, q harness.TaskArtifactQuery) (harness.TaskArtifactPage, error) {
	return h.runtime.TaskArtifacts(ctx, actor, q)
}

// TaskArtifacts enumerates all committed artifacts of a logical task node.
func (c *Client) TaskArtifacts(ctx context.Context, sessionID string, q harness.TaskArtifactQuery) (harness.TaskArtifactPage, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.TaskArtifactPage{}, err
	}
	defer done()
	return c.background.TaskArtifacts(ctx, actor, q)
}

// ReadTaskArtifact reads a verified, bounded chunk of a committed task output.
// Root/node attribution is resolved by the host, never from a client AssetRef.
func (c *Client) ReadTaskArtifact(ctx context.Context, sessionID string, request harness.TaskArtifactReadRequest) (harness.TaskArtifactChunk, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.TaskArtifactChunk{}, err
	}
	defer done()
	return c.background.ReadTaskArtifact(ctx, actor, request)
}
