package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func (c *Client) managementGeneration(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	switch r.Operation {
	case "skill.generation.status":
		if err := proposalFields(r); err != nil {
			return nil, err
		}
		return c.evolution.GenerationStatus(), nil
	case "skill.generation.prepare":
		if err := proposalFields(r, "target"); err != nil {
			return nil, err
		}
		var target harness.SkillTarget
		if err := managementSkillObject(r.Fields, "target", &target, "source_id", "source_identity", "name", "scope"); err != nil {
			return nil, err
		}
		return c.evolution.PrepareGeneration(ctx, target)
	case "skill.generation.list":
		if err := proposalFields(r, "source_id", "name", "cursor", "limit"); err != nil {
			return nil, err
		}
		q, err := managementProposalQuery(r.Fields)
		if err != nil {
			return nil, err
		}
		return c.evolution.QueryGeneration(ctx, q)
	case "skill.generation.enqueue":
		if err := proposalFields(r, "target", "expected_head", "request_key", "reason"); err != nil {
			return nil, err
		}
		var target harness.SkillTarget
		if err := managementSkillObject(r.Fields, "target", &target, "source_id", "source_identity", "name", "scope"); err != nil {
			return nil, err
		}
		var head harness.SkillHead
		if err := managementSkillObject(r.Fields, "expected_head", &head, "version", "state_version", "exists", "enabled", "managed"); err != nil {
			return nil, err
		}
		current, _, _, err := c.evolution.Head(ctx, target.SourceID, target.Name)
		if err != nil {
			return nil, err
		}
		if current != target {
			return nil, harness.ErrPermissionDenied
		}
		key, err := managementID(r.Fields, "request_key")
		if err != nil {
			return nil, err
		}
		reason, err := optionalProposalString(r.Fields, "reason", 4000)
		if err != nil {
			return nil, err
		}
		return c.evolution.EnqueueGeneration(ctx, harness.SkillGenerationRequest{SourceID: target.SourceID, Name: target.Name, RequestKey: key, Reason: reason, Author: "desktop", Base: head})
	case "skill.generation.get", "skill.generation.cancel", "skill.generation.receipts":
		allowed := []string{"job_id"}
		if r.Operation == "skill.generation.cancel" {
			allowed = append(allowed, "expected_state_version")
		}
		if r.Operation == "skill.generation.receipts" {
			allowed = append(allowed, "after_sequence", "limit")
		}
		if err := proposalFields(r, allowed...); err != nil {
			return nil, err
		}
		id, err := managementID(r.Fields, "job_id")
		if err != nil {
			return nil, err
		}
		if r.Operation == "skill.generation.get" {
			return c.evolution.GetGeneration(ctx, id)
		}
		if r.Operation == "skill.generation.cancel" {
			version, err := optionalProposalInteger(r.Fields, "expected_state_version", 0)
			if err != nil || version == 0 {
				return nil, managementInvalid("expected_state_version is required")
			}
			if err = c.evolution.CancelGeneration(ctx, id, version); err != nil {
				return nil, err
			}
			return map[string]any{"cancelled": true}, nil
		}
		after, err := optionalProposalInteger(r.Fields, "after_sequence", 0)
		if err != nil {
			return nil, err
		}
		limit, err := optionalProposalInteger(r.Fields, "limit", 50)
		if err != nil {
			return nil, err
		}
		if limit > 100 {
			return nil, managementInvalid("limit must be 1..100")
		}
		receipts, err := c.evolution.GenerationReceipts(ctx, id, after, int(limit))
		if err != nil {
			return nil, err
		}
		next := after
		if len(receipts) > 0 {
			next = receipts[len(receipts)-1].Sequence
		}
		return map[string]any{"receipts": receipts, "last_sequence": next}, nil
	}
	return nil, managementInvalid("unsupported skill generation operation")
}
