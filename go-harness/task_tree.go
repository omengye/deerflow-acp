package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (h *backgroundHost) TaskTree(ctx context.Context, actor harness.TaskActor, query harness.TaskTreeQuery) (harness.TaskTreePage, error) {
	return h.runtime.TaskTree(ctx, actor, query)
}

func (h *backgroundHost) TaskTreeNode(ctx context.Context, actor harness.TaskActor, rootID, nodeID string) (harness.TaskTreeNode, error) {
	return h.runtime.TaskTreeNode(ctx, actor, rootID, nodeID)
}

func (h *backgroundHost) CancelTaskTree(ctx context.Context, actor harness.TaskActor, request harness.TaskTreeCancelRequest) (out harness.TaskTreeNode, err error) {
	defer func() { err = backgroundAPIError(err) }()
	return h.runtime.CancelTaskTree(ctx, actor, request)
}

var _ harness.TaskTreeController = (*backgroundHost)(nil)
var _ harness.TaskTreeCanceller = (*backgroundHost)(nil)

// TaskTree returns a live page of root and logical role nodes. Resume attempts
// retain logical identity. Finish pagination before pruning a cached tree.
func (c *Client) TaskTree(ctx context.Context, sessionID string, query harness.TaskTreeQuery) (harness.TaskTreePage, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.TaskTreePage{}, err
	}
	defer done()
	return c.background.TaskTree(ctx, actor, query)
}

func (c *Client) TaskTreeNode(ctx context.Context, sessionID, rootID, nodeID string) (harness.TaskTreeNode, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.TaskTreeNode{}, err
	}
	defer done()
	return c.background.TaskTreeNode(ctx, actor, rootID, nodeID)
}

// CancelTaskTree persistently stops a node and its descendants. A root node
// stops the whole tree, including descendants absent from the caller's cache.
// ExpectedVersion must match the inspected node; cancellation may still be
// draining when the returned node is read.
func (c *Client) CancelTaskTree(ctx context.Context, sessionID string, request harness.TaskTreeCancelRequest) (harness.TaskTreeNode, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.TaskTreeNode{}, err
	}
	defer done()
	return c.background.CancelTaskTree(ctx, actor, request)
}
