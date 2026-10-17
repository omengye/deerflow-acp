package deerflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func proposalManagementError(err error) error {
	if err == nil {
		return nil
	}
	var known *localhost.ManagementError
	if errors.As(err, &known) {
		return err
	}
	code, message := "operation_failed", "skill management operation failed"
	var publicationConflict *skills.PublicationConflict
	switch {
	case errors.As(err, &publicationConflict):
		code, message = publicationConflict.Code, publicationConflict.Error()
	case errors.Is(err, harness.ErrReconciliationRequired):
		code, message = "publication_reconciliation_required", "Approved publication has not completed; inspect its durable result before retrying."
	case errors.Is(err, harness.ErrSkillConflict), errors.Is(err, skills.ErrPublicationConflict), errors.Is(err, skillmaintenance.ErrConflict):
		code, message = "conflict", "Skill state changed; refresh the current record before retrying."
	case errors.Is(err, harness.ErrSkillValidation):
		code, message = "validation_failed", "Candidate checks are incomplete or failed."
	case errors.Is(err, harness.ErrInvalidInput):
		code, message = "invalid_request", "Invalid skill management request."
	case errors.Is(err, skillmaintenance.ErrInvalid):
		code, message = "invalid_request", "Invalid skill maintenance request."
	case errors.Is(err, skillmaintenance.ErrBudget):
		code, message = "resource_limit", "Skill maintenance queue or budget limit reached."
	case errors.Is(err, harness.ErrSkillFeedbackLimit):
		code, message = "resource_limit", "Skill feedback revision limit reached."
	case errors.Is(err, harness.ErrSkillMissingReportLimit):
		code, message = "resource_limit", "Missing skill report revision limit reached."
	case errors.Is(err, harness.ErrPermissionDenied):
		code, message = "permission_denied", "The skill source is not currently authorized."
	case errors.Is(err, harness.ErrNotFound):
		code, message = "not_found", "The requested skill record was not found."
	}
	return &localhost.ManagementError{Code: code, Message: message}
}

func proposalFields(request localhost.ManagementRequest, allowed ...string) error {
	for key := range request.Fields {
		if key != "operation" && !slices.Contains(allowed, key) {
			return managementInvalid("unexpected skill management field: " + key)
		}
	}
	return nil
}

func optionalProposalString(fields map[string]json.RawMessage, key string, maximum int) (string, error) {
	raw, exists := fields[key]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) > maximum || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return "", managementInvalid("invalid " + key)
	}
	return strings.TrimSpace(value), nil
}

func optionalProposalInteger(fields map[string]json.RawMessage, key string, fallback int64) (int64, error) {
	raw, exists := fields[key]
	if !exists {
		return fallback, nil
	}
	var value int64
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < 1 {
		return 0, managementInvalid("invalid " + key)
	}
	return value, nil
}

func managementProposalQuery(fields map[string]json.RawMessage) (harness.SkillQuery, error) {
	var q harness.SkillQuery
	var err error
	for key, out := range map[string]*string{"source_id": &q.SourceID, "name": &q.Name, "status": &q.Status, "cursor": &q.Cursor} {
		max := 128
		if key == "cursor" {
			max = 2048
		}
		if *out, err = optionalProposalString(fields, key, max); err != nil {
			return q, err
		}
	}
	limit, err := optionalProposalInteger(fields, "limit", 50)
	if err != nil {
		return q, err
	}
	if limit > 100 {
		return q, managementInvalid("limit must be 1..100")
	}
	q.Limit = int(limit)
	if raw, ok := fields["needs_attention"]; ok {
		var value bool
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return q, managementInvalid("invalid needs_attention")
		}
		q.NeedsAttention = &value
	}
	return q, nil
}

// Keep legacy desktop fields while exposing complete source/head identities
// and state versions to clients that can submit stronger preconditions.
func proposalProjection(p harness.SkillProposal) map[string]any {
	data, _ := json.Marshal(p)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	result["base_sha256"] = nil
	if p.Base.Exists {
		result["base_sha256"] = p.Base.Version
	}
	state, code, retry := "not_approved", p.PublicationError, false
	if p.Approval != nil {
		switch p.Status {
		case "published":
			state = "published"
		case "publishing":
			state, retry = "needs_reconciliation", true
		case "pending_review":
			state, retry = "approved_pending", true
		default:
			state = "blocked"
		}
	} else if p.Status == "published" || p.Status == "publishing" {
		state = p.Status
	}
	result["publication"] = map[string]any{"state": state, "error_code": code, "retryable": retry}
	return result
}

func proposalProjections(proposals []harness.SkillProposal) []map[string]any {
	result := make([]map[string]any, 0, len(proposals))
	for _, p := range proposals {
		result = append(result, proposalProjection(p))
	}
	return result
}

func (c *Client) managementProposals(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	switch request.Operation {
	case "proposal.publish_retry":
		if err := proposalFields(request, "proposal_id", "expected_approval_version"); err != nil {
			return nil, err
		}
		id, err := managementID(request.Fields, "proposal_id")
		if err != nil {
			return nil, err
		}
		state, err := optionalProposalInteger(request.Fields, "expected_approval_version", 0)
		if err != nil || state < 1 {
			return nil, managementInvalid("publication retry requires the original approval version")
		}
		p, err := c.evolution.RetryPublication(ctx, id, state)
		if err != nil {
			if p.ID == id && p.Approval != nil && p.Approval.Review.ExpectedStateVersion == state {
				mapped := proposalManagementError(err).(*localhost.ManagementError)
				projection := proposalProjection(p)
				projection["diff"] = ""
				mapped.Details = map[string]any{"proposal": projection}
				return nil, mapped
			}
			return nil, err
		}
		_, _, catalog, err := c.evolution.Head(ctx, p.Target.SourceID, p.SkillName)
		return map[string]any{"proposal": proposalProjection(p), "catalog_version": catalog}, err
	case "proposal.source_review":
		if err := proposalFields(request, "proposal_id", "expected_state_version", "request_key"); err != nil {
			return nil, err
		}
		id, err := managementID(request.Fields, "proposal_id")
		if err != nil {
			return nil, err
		}
		state, err := optionalProposalInteger(request.Fields, "expected_state_version", 0)
		if err != nil || state < 1 {
			return nil, managementInvalid("source review requires an observed state version")
		}
		key, err := managementID(request.Fields, "request_key")
		if err != nil {
			return nil, err
		}
		p, err := c.evolution.ReviewSource(ctx, id, state, key)
		if err != nil {
			return nil, err
		}
		_, _, catalog, err := c.evolution.Head(ctx, p.Target.SourceID, p.SkillName)
		return map[string]any{"proposal": proposalProjection(p), "catalog_version": catalog}, err
	case "proposal.list", "proposal.history":
		allowed := []string{"source_id", "name", "status", "cursor", "limit"}
		if request.Operation == "proposal.history" {
			allowed = append(allowed, "revision_cursor")
		} else {
			allowed = append(allowed, "needs_attention")
		}
		if err := proposalFields(request, allowed...); err != nil {
			return nil, err
		}
		query, err := managementProposalQuery(request.Fields)
		if err != nil {
			return nil, err
		}
		page, err := c.evolution.QueryProposals(ctx, query)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"proposals": proposalProjections(page.Proposals), "catalog_version": page.CatalogVersion, "next_cursor": page.Next, "truncated": page.Next != ""}
		if request.Operation == "proposal.list" {
			return result, nil
		}
		query.Status = ""
		query.Cursor, err = optionalProposalString(request.Fields, "revision_cursor", 2048)
		if err != nil {
			return nil, err
		}
		history, err := c.evolution.QueryHistory(ctx, query)
		if err != nil {
			return nil, err
		}
		result["revisions"] = history.Publications
		result["revision_next_cursor"] = history.Next
		result["revision_catalog_version"] = history.CatalogVersion
		result["truncated"] = page.Next != "" || history.Next != ""
		// Discovery status/history use their explicit endpoints; the legacy
		// containers do not stand in for selection records or probation results.
		result["signals"] = []any{}
		result["probations"] = map[string]any{}
		result["capabilities"] = map[string]bool{"review": true, "rollback": true, "generation": c.evolution.GenerationEnabled(), "discovery": c.evolution.DiscoveryEnabled(), "probation": false}
		return result, nil
	case "proposal.get", "proposal.approve", "proposal.reject":
		allowed := []string{"proposal_id"}
		if request.Operation != "proposal.get" {
			allowed = append(allowed, "expected_state_version", "expected_base_sha256", "note")
		}
		if err := proposalFields(request, allowed...); err != nil {
			return nil, err
		}
		id, err := managementID(request.Fields, "proposal_id")
		if err != nil {
			return nil, err
		}
		p, err := c.evolution.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if request.Operation == "proposal.get" {
			_, head, catalog, err := c.evolution.Head(ctx, p.Target.SourceID, p.SkillName)
			if err != nil {
				return nil, err
			}
			result := proposalProjection(p)
			result["current_head"] = head
			result["catalog_version"] = catalog
			return result, nil
		}
		state, err := optionalProposalInteger(request.Fields, "expected_state_version", p.StateVersion)
		if err != nil {
			return nil, err
		}
		// Legacy duplicate reviews can recover the original state version. The
		// application service still verifies actor, note and idempotency digest.
		if _, explicit := request.Fields["expected_state_version"]; !explicit {
			if p.Approval != nil && request.Operation == "proposal.approve" {
				state = p.Approval.Review.ExpectedStateVersion
			} else if p.Status == "published" || p.Status == "publishing" || p.Status == "rejected" {
				state--
			}
		}
		base, err := optionalProposalString(request.Fields, "expected_base_sha256", 64)
		if err != nil {
			return nil, err
		}
		if base != "" {
			if decoded, err := hex.DecodeString(base); err != nil || len(decoded) != 32 {
				return nil, managementInvalid("expected_base_sha256 must be a SHA-256 digest")
			}
			if !strings.EqualFold(base, p.Base.Version) {
				return nil, harness.ErrSkillConflict
			}
		}
		note, err := optionalProposalString(request.Fields, "note", 4000)
		if err != nil {
			return nil, err
		}
		review := harness.SkillReview{ExpectedStateVersion: state, Actor: "desktop", Note: note}
		if request.Operation == "proposal.approve" {
			p, err = c.evolution.Approve(ctx, id, review)
		} else {
			p, err = c.evolution.Reject(ctx, id, review)
		}
		if err != nil {
			if request.Operation == "proposal.approve" && p.ID == id && p.Approval != nil && p.Approval.Review == review {
				mapped := proposalManagementError(err).(*localhost.ManagementError)
				projection := proposalProjection(p)
				projection["diff"] = ""
				mapped.Details = map[string]any{"proposal": projection}
				return nil, mapped
			}
			return nil, err
		}
		_, _, catalog, err := c.evolution.Head(ctx, p.Target.SourceID, p.SkillName)
		return map[string]any{"proposal": proposalProjection(p), "catalog_version": catalog}, err
	case "proposal.rollback":
		return c.managementSkillRollback(ctx, request)
	}
	return nil, managementInvalid("unsupported skill management operation")
}

func (c *Client) managementSkillRollback(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	if err := proposalFields(request, "source_id", "name", "version", "expected_head", "request_key", "note", "target"); err != nil {
		return nil, err
	}
	name, err := managementID(request.Fields, "name")
	if err != nil {
		return nil, err
	}
	revision, err := optionalProposalInteger(request.Fields, "version", 0)
	if err != nil || revision < 1 {
		return nil, managementInvalid("version must be a positive skill revision")
	}
	source, err := optionalProposalString(request.Fields, "source_id", 128)
	if err != nil {
		return nil, err
	}
	if source == "" {
		sources, err := c.skills.PublicationSources()
		if err != nil {
			return nil, err
		}
		for _, candidate := range sources {
			history, err := c.evolution.History(ctx, candidate.SourceID, name, 0, 1)
			if err != nil {
				return nil, err
			}
			if len(history) == 0 {
				continue
			}
			if source != "" {
				return nil, managementInvalid("source_id is required for an ambiguous skill name")
			}
			source = candidate.SourceID
		}
		if source == "" {
			return nil, harness.ErrNotFound
		}
	}
	target, head, _, err := c.evolution.Head(ctx, source, name)
	if err != nil {
		return nil, err
	}
	if _, supplied := request.Fields["target"]; supplied {
		var expected harness.SkillTarget
		if err := managementSkillObject(request.Fields, "target", &expected, "source_id", "source_identity", "name", "scope"); err != nil {
			return nil, err
		}
		if expected != target {
			return nil, harness.ErrPermissionDenied
		}
		if err := managementSkillObject(request.Fields, "expected_head", &head, "version", "state_version", "exists", "enabled", "managed"); err != nil {
			return nil, err
		}
		if _, err := managementID(request.Fields, "request_key"); err != nil {
			return nil, err
		}
	}
	if raw, ok := request.Fields["expected_head"]; ok {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || decoder.Decode(&head) != nil || head.StateVersion < 1 {
			return nil, managementInvalid("invalid expected_head")
		}
	}
	note, err := optionalProposalString(request.Fields, "note", 4000)
	if err != nil {
		return nil, err
	}
	key, err := optionalProposalString(request.Fields, "request_key", 256)
	if err != nil {
		return nil, err
	}
	if key == "" {
		data, _ := json.Marshal([]any{source, name, revision, head, note})
		key = fmt.Sprintf("legacy-%x", sha256.Sum256(data))
	}
	p, err := c.evolution.Rollback(ctx, harness.SkillRollback{SourceID: source, Name: name, Revision: revision, Expected: head, RequestKey: key, Actor: "desktop", Note: note})
	if err != nil {
		return nil, err
	}
	_, _, catalog, err := c.evolution.Head(ctx, source, name)
	return map[string]any{"revision": p, "catalog_version": catalog}, err
}
