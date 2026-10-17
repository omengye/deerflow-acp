package deerflow

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

// Unlike the legacy proposal protocol these new commands require explicit
// complete identities/preconditions. A missing boolean is not an observed false.
func managementSkillObject(fields map[string]json.RawMessage, key string, dst any, required ...string) error {
	raw := fields[key]
	var values map[string]json.RawMessage
	if len(raw) == 0 || len(raw) > 16384 || json.Unmarshal(raw, &values) != nil || values == nil {
		return managementInvalid("invalid " + key)
	}
	for _, name := range required {
		v := bytes.TrimSpace(values[name])
		if len(v) == 0 || bytes.Equal(v, []byte("null")) {
			return managementInvalid("missing " + key + "." + name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil {
		return managementInvalid("invalid " + key)
	}
	return nil
}

func (c *Client) managementSkillEdits(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	fields := []string{"target"}
	if request.Operation == "skill.edits.reconcile" {
		fields = append(fields, "edit_id", "expected_head", "action")
	}
	if err := proposalFields(request, fields...); err != nil {
		return nil, err
	}
	var target harness.SkillTarget
	if err := managementSkillObject(request.Fields, "target", &target, "source_id", "source_identity", "name", "scope"); err != nil {
		return nil, err
	}
	if request.Operation == "skill.edits.list" {
		return c.skills.HostEdits(ctx, target)
	}
	var head harness.SkillHead
	if err := managementSkillObject(request.Fields, "expected_head", &head, "version", "state_version", "exists", "enabled", "managed"); err != nil {
		return nil, err
	}
	id, err := managementID(request.Fields, "edit_id")
	if err != nil {
		return nil, err
	}
	action, err := managementID(request.Fields, "action")
	if err != nil {
		return nil, err
	}
	return c.skills.ReconcileEdit(ctx, target, id, head, action, "desktop")
}

func (c *Client) managementSkills(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	if request.Operation == "skill.list" {
		if err := proposalFields(request, "source_id", "name", "status", "cursor", "limit"); err != nil {
			return nil, err
		}
		query, err := managementProposalQuery(request.Fields)
		if err != nil {
			return nil, err
		}
		return c.skills.QueryCatalog(ctx, query)
	}
	allowed := []string{"target"}
	if request.Operation != "skill.get" {
		allowed = append(allowed, "expected_head")
	}
	switch request.Operation {
	case "skill.set_enabled":
		allowed = append(allowed, "enabled", "request_key")
	case "skill.export.retry":
		allowed = append(allowed, "expected_export_token", "request_key")
	}
	if err := proposalFields(request, allowed...); err != nil {
		return nil, err
	}
	var target harness.SkillTarget
	if err := managementSkillObject(request.Fields, "target", &target, "source_id", "source_identity", "name", "scope"); err != nil {
		return nil, err
	}
	if request.Operation == "skill.get" {
		return c.skills.GetCatalogSkill(ctx, target)
	}
	var head harness.SkillHead
	if err := managementSkillObject(request.Fields, "expected_head", &head, "version", "state_version", "exists", "enabled", "managed"); err != nil {
		return nil, err
	}
	if request.Operation == "skill.imports.refresh" {
		return c.skills.RefreshCatalogImport(ctx, target, head)
	}
	key, err := managementID(request.Fields, "request_key")
	if err != nil {
		return nil, err
	}
	if request.Operation == "skill.set_enabled" {
		var enabled *bool
		if json.Unmarshal(request.Fields["enabled"], &enabled) != nil || enabled == nil {
			return nil, managementInvalid("enabled must be a boolean")
		}
		return c.skills.SetEnabledChecked(ctx, harness.SkillEnablement{Target: target, Expected: head, Enabled: *enabled, RequestKey: key, Actor: "desktop"})
	}
	token, err := managementID(request.Fields, "expected_export_token")
	if err != nil {
		return nil, err
	}
	return c.skills.RetryExportChecked(ctx, harness.SkillExportRetry{Target: target, Expected: head, ExpectedToken: token, RequestKey: key, Actor: "desktop"})
}

func (c *Client) managementSkillPublications(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	fields := []string{"target"}
	if request.Operation == "skill.publications.reconcile" {
		fields = append(fields, "publication_id", "expected_head", "action")
	}
	if err := proposalFields(request, fields...); err != nil {
		return nil, err
	}
	var target harness.SkillTarget
	if err := managementSkillObject(request.Fields, "target", &target, "source_id", "source_identity", "name", "scope"); err != nil {
		return nil, err
	}
	if request.Operation == "skill.publications.list" {
		return c.skills.HostFilePublications(ctx, target)
	}
	var head harness.SkillHead
	if err := managementSkillObject(request.Fields, "expected_head", &head, "version", "state_version", "exists", "enabled", "managed"); err != nil {
		return nil, err
	}
	id, err := managementID(request.Fields, "publication_id")
	if err != nil {
		return nil, err
	}
	action, err := managementID(request.Fields, "action")
	if err != nil {
		return nil, err
	}
	if err = c.skills.ReconcilePublication(ctx, target, id, head, action, "desktop"); err != nil {
		return nil, err
	}
	return c.skills.GetCatalogSkill(ctx, target)
}
