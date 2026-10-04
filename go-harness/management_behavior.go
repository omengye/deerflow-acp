package deerflow

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func (c *Client) managementBehavior(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	fields := []string{"proposal_id"}
	if request.Operation == "proposal.behavior.receipts" {
		fields = append(fields, "after_sequence", "limit")
	}
	if err := proposalFields(request, fields...); err != nil {
		return nil, err
	}
	id, err := managementID(request.Fields, "proposal_id")
	if err != nil {
		return nil, err
	}
	if request.Operation == "proposal.behavior" {
		return c.evolution.BehaviorEvaluation(ctx, id)
	}
	if request.Operation != "proposal.behavior.receipts" {
		return nil, managementInvalid("unsupported behavior operation")
	}
	var after int64
	if raw, exists := request.Fields["after_sequence"]; exists {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &after) != nil || after < 0 {
			return nil, managementInvalid("invalid after_sequence")
		}
	}
	limit, err := optionalProposalInteger(request.Fields, "limit", 50)
	if err != nil {
		return nil, err
	}
	if limit > 100 {
		return nil, managementInvalid("limit must be 1..100")
	}
	receipts, err := c.evolution.BehaviorReceipts(ctx, id, after, int(limit))
	if err != nil {
		return nil, err
	}
	next := after
	if len(receipts) > 0 {
		next = receipts[len(receipts)-1].Sequence
	}
	return map[string]any{"proposal_id": id, "receipts": receipts, "last_sequence": next}, nil
}
