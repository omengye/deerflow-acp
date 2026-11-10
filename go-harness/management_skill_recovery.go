package deerflow

import (
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func (c *Client) managementSkillRecovery(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	fields := []string{"target", "history_database"}
	if request.Operation == "skill.recovery.apply" {
		fields = append(fields, "expected_version", "expected_receipt_sha256", "enabled", "request_key")
	}
	if err := proposalFields(request, fields...); err != nil {
		return nil, err
	}
	var target harness.SkillTarget
	if err := managementSkillObject(request.Fields, "target", &target, "source_id", "source_identity", "name", "scope"); err != nil {
		return nil, err
	}
	history, err := optionalProposalString(request.Fields, "history_database", 4096)
	if err != nil {
		return nil, err
	}
	if request.Operation == "skill.recovery.plan" {
		return c.skills.PlanRecovery(ctx, target, history)
	}
	version, err := managementID(request.Fields, "expected_version")
	if err != nil {
		return nil, err
	}
	receipt, err := managementID(request.Fields, "expected_receipt_sha256")
	if err != nil {
		return nil, err
	}
	key, err := managementID(request.Fields, "request_key")
	if err != nil {
		return nil, err
	}
	var enabled *bool
	if json.Unmarshal(request.Fields["enabled"], &enabled) != nil || enabled == nil {
		return nil, managementInvalid("enabled must be a boolean")
	}
	return c.skills.RecoverPublished(ctx, harness.SkillRecovery{Target: target, HistoryDatabase: history, Version: version, ReceiptSHA256: receipt, Enabled: *enabled, RequestKey: key, Actor: "host-recovery"})
}
