package deerflow

import (
	"bytes"
	"context"

	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func (c *Client) managementDiscovery(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	switch request.Operation {
	case "skill.discovery.status":
		if err := proposalFields(request); err != nil {
			return nil, err
		}
		return c.evolution.DiscoveryStatus(ctx)
	case "skill.discovery.list":
		if err := proposalFields(request, "source_id", "name", "cursor", "limit"); err != nil {
			return nil, err
		}
		for key, raw := range request.Fields {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil, managementInvalid(key + " must not be null")
			}
		}
		query, err := managementProposalQuery(request.Fields)
		if err != nil {
			return nil, err
		}
		return c.evolution.QueryDiscoveries(ctx, query)
	default:
		return nil, managementInvalid("unsupported skill discovery operation")
	}
}
