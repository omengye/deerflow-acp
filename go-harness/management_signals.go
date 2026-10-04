package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func (c *Client) managementSignals(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	switch request.Operation {
	case "skill.signals.list":
		if err := proposalFields(request, "source_id", "name", "cursor", "limit"); err != nil {
			return nil, err
		}
		query, err := managementProposalQuery(request.Fields)
		if err != nil {
			return nil, err
		}
		return c.evolution.QuerySignals(ctx, query)
	case "skill.signals.get":
		if err := proposalFields(request, "signal_id", "cursor", "limit"); err != nil {
			return nil, err
		}
		id, err := managementID(request.Fields, "signal_id")
		if err != nil {
			return nil, err
		}
		cursor, err := optionalProposalString(request.Fields, "cursor", 2048)
		if err != nil {
			return nil, err
		}
		limit, err := optionalProposalInteger(request.Fields, "limit", 50)
		if err != nil {
			return nil, err
		}
		if limit > 100 {
			return nil, managementInvalid("limit must be 1..100")
		}
		return c.evolution.SignalEvidence(ctx, id, cursor, int(limit))
	case "skill.collector.status":
		if err := proposalFields(request); err != nil {
			return nil, err
		}
		return c.evolution.CollectorStatus(ctx)
	case "skill.collector.jobs":
		if err := proposalFields(request, "after_outcome_id", "limit"); err != nil {
			return nil, err
		}
		after, err := optionalProposalInteger(request.Fields, "after_outcome_id", 0)
		if err != nil {
			return nil, err
		}
		limit, err := optionalProposalInteger(request.Fields, "limit", 50)
		if err != nil {
			return nil, err
		}
		if limit > 100 {
			return nil, managementInvalid("limit must be 1..100")
		}
		return c.evolution.QueryCollectionJobs(ctx, after, int(limit))
	case "skill.collector.retry":
		if err := proposalFields(request, "outcome_id", "expected_state_version"); err != nil {
			return nil, err
		}
		id, err := optionalProposalInteger(request.Fields, "outcome_id", 0)
		if err != nil || id == 0 {
			return nil, managementInvalid("outcome_id is required")
		}
		version, err := optionalProposalInteger(request.Fields, "expected_state_version", 0)
		if err != nil || version == 0 {
			return nil, managementInvalid("expected_state_version is required")
		}
		if err = c.evolution.RetryCollection(ctx, id, version); err != nil {
			return nil, err
		}
		return map[string]any{"queued": true}, nil
	}
	return nil, managementInvalid("unsupported skill evidence operation")
}
