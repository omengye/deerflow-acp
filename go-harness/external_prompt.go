package deerflow

import (
	"context"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	acpclient "github.com/omengye/deerflow-acp/go-harness/internal/acp/client"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

// PendingExternalPrompt identifies a remote prompt that cannot be safely
// resent. The host must inspect the remote session and local tool receipt.
func (c *Client) PendingExternalPrompt(ctx context.Context, sessionID, agent string) (harness.ExternalPromptState, error) {
	done, err := c.operation()
	if err != nil {
		return harness.ExternalPromptState{}, err
	}
	defer done()
	if c.service.ExternalPromptPending == nil {
		return harness.ExternalPromptState{}, fmt.Errorf("%w: external ACP agents are disabled", harness.ErrInvalidInput)
	}
	return c.service.ExternalPromptPending(ctx, c.owner, sessionID, agent)
}

// AcknowledgeExternalPrompt clears an unresolved marker only after the exact
// parent tool receipt is committed as completed or explicitly reconciled.
// It never calls or replays the remote agent.
func (c *Client) AcknowledgeExternalPrompt(ctx context.Context, sessionID, agent, promptID string) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	if c.service.ExternalPromptAcknowledge == nil {
		return fmt.Errorf("%w: external ACP agents are disabled", harness.ErrInvalidInput)
	}
	return c.service.ExternalPromptAcknowledge(ctx, c.owner, sessionID, agent, promptID)
}

func pendingExternalPrompt(ctx context.Context, service *hr.Service, root string, agents map[string]harness.ACPAgentConfig, owner, sessionID, agent string) (harness.ExternalPromptState, error) {
	if _, ok := agents[agent]; !ok {
		return harness.ExternalPromptState{}, fmt.Errorf("%w: external ACP agent is not configured", harness.ErrInvalidInput)
	}
	ctx, release, err := service.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return harness.ExternalPromptState{}, err
	}
	defer release()
	return acpclient.PendingPrompt(root, sessionID, agent)
}

func acknowledgeExternalPrompt(ctx context.Context, service *hr.Service, root string, agents map[string]harness.ACPAgentConfig, owner, sessionID, agent, promptID string) error {
	if _, ok := agents[agent]; !ok || promptID == "" {
		return fmt.Errorf("%w: external ACP agent or prompt is invalid", harness.ErrInvalidInput)
	}
	ctx, release, err := service.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return err
	}
	defer release()
	pending, err := acpclient.PendingPrompt(root, sessionID, agent)
	if err != nil {
		return err
	}
	if pending.PromptID != promptID || pending.RunID == "" || pending.ToolCallID == "" || pending.ArgumentsSHA == "" {
		return harness.ErrReconciliationRequired
	}
	receipts, err := service.Store.ListToolReceipts(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		if receipt.RunID != pending.RunID || receipt.ToolCallID != pending.ToolCallID {
			continue
		}
		if receipt.ToolName != "invoke_acp_agent" || receipt.ArgumentsDigest != pending.ArgumentsSHA {
			return harness.ErrReceiptConflict
		}
		if receipt.State != harness.ReceiptCompleted && !(receipt.State == harness.ReceiptNoEffect && receipt.Review != nil && receipt.Review.PreviousState == harness.ReceiptUncertain) {
			return harness.ErrReconciliationRequired
		}
		return acpclient.AcknowledgePrompt(root, sessionID, agent, pending)
	}
	return harness.ErrReceiptConflict
}
