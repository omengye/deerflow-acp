package deerflow

import (
	"bytes"
	"context"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func (c *Client) managementAutoPatch(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	switch r.Operation {
	case "skill.auto_patch.status":
		if err := proposalFields(r); err != nil {
			return nil, err
		}
		return c.evolution.AutoPatchStatus(ctx)
	case "skill.auto_patch.list":
		if err := proposalFields(r, "source_id", "name", "cursor", "limit"); err != nil {
			return nil, err
		}
		for key, raw := range r.Fields {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil, managementInvalid(key + " must not be null")
			}
		}
		q, err := managementProposalQuery(r.Fields)
		if err != nil {
			return nil, err
		}
		return c.evolution.QueryAutoPatch(ctx, q)
	default:
		return nil, managementInvalid("unsupported automatic skill publication operation")
	}
}
