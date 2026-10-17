package deerflow

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

// Verification reads the next-turn snapshot, not an old run or executable script.
func (c *Client) managementSkillVerify(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	if err := proposalFields(request, "target", "session_id", "expected_version"); err != nil {
		return nil, err
	}
	var target harness.SkillTarget
	if err := managementSkillObject(request.Fields, "target", &target, "source_id", "source_identity", "name", "scope"); err != nil {
		return nil, err
	}
	if _, err := c.skills.GetCatalogSkill(ctx, target); err != nil {
		return nil, err
	}
	id, err := managementID(request.Fields, "session_id")
	if err != nil {
		return nil, err
	}
	x, err := c.service.Store.ManagementSession(ctx, id)
	if err != nil {
		return nil, err
	}
	var expected string
	if raw, ok := request.Fields["expected_version"]; ok {
		if json.Unmarshal(raw, &expected) != nil || len(expected) != 64 {
			return nil, managementInvalid("expected_version must be a SHA-256 version")
		}
		if _, err := hex.DecodeString(expected); err != nil {
			return nil, managementInvalid("expected_version must be a SHA-256 version")
		}
	}
	result := map[string]any{"available": false, "body_readable": false, "execution_checked": false, "scope": "next_turn", "session_id": id, "target": target, "data_dir": c.dataDir}
	if !c.skillReadAllowed {
		result["reason"] = "skill_tools_disabled"
		return result, nil
	}
	selection := c.skillSelection
	selection.Workspace = x.CWD
	snapshot, err := c.skills.Snapshot(ctx, selection)
	if err != nil {
		return nil, err
	}
	for _, ref := range snapshot.Refs() {
		if ref.Name != target.Name || ref.SourceID != target.SourceID || ref.SourceIdentity != target.SourceIdentity {
			continue
		}
		result["version"] = ref.Version
		if expected != "" && ref.Version != expected {
			result["reason"] = "version_changed"
			return result, nil
		}
		if _, err = snapshot.Get(ctx, ref.Name); err != nil {
			result["reason"] = "body_unreadable"
			return result, nil
		}
		result["available"], result["body_readable"], result["reason"] = true, true, "verified"
		return result, nil
	}
	result["reason"] = "not_enabled_or_not_visible"
	return result, nil
}
