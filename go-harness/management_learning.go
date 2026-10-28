package deerflow

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

func learningControlRequest(raw json.RawMessage) (harness.SkillLearningControlRequest, error) {
	var control harness.SkillLearningControlRequest
	if len(raw) > 1024 {
		return control, managementInvalid("invalid learning control")
	}
	canonical, err := skillbehavior.CanonicalJSON(raw)
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(canonical, &fields) != nil || len(fields) < 2 || len(fields) > 3 {
		return control, managementInvalid("invalid learning control")
	}
	for key, value := range fields {
		if key != "contribute" && key != "expected_epoch" && key != "workspace_shared" || bytes.Equal(value, []byte("null")) {
			return control, managementInvalid("invalid learning control field")
		}
	}
	for _, key := range []string{"contribute", "expected_epoch"} {
		if len(fields[key]) == 0 || bytes.Equal(fields[key], []byte("null")) {
			return control, managementInvalid("missing learning control field")
		}
	}
	if json.Unmarshal(canonical, &control) != nil || control.ExpectedEpoch < 0 {
		return control, managementInvalid("invalid learning control")
	}
	return control, nil
}

func learningReviewRequest(raw json.RawMessage, adjudicate bool) (any, error) {
	if len(raw) > 8<<10 {
		return nil, managementInvalid("learning review exceeds bounds")
	}
	canonical, err := skillbehavior.CanonicalJSON(raw)
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(canonical, &fields) != nil {
		return nil, managementInvalid("invalid learning review")
	}
	keys := []string{"pattern_id", "expected_pattern_revision", "expected_state_version", "scope", "request_key", "ignored"}
	if adjudicate {
		keys = append(keys[:5], "target", "expected_head", "overlay_base", "relation", "reason")
	}
	if len(fields) != len(keys) {
		return nil, managementInvalid("invalid learning review fields")
	}
	for _, key := range keys {
		value := fields[key]
		if len(value) == 0 || key != "overlay_base" && bytes.Equal(value, []byte("null")) {
			return nil, managementInvalid("missing learning review field")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if adjudicate {
		var result harness.SkillLearningPatternAdjudicationRequest
		if decoder.Decode(&result) != nil {
			return nil, managementInvalid("invalid learning adjudication")
		}
		if err := learningReviewObject(fields, "target", &result.Target, []string{"workspace"}, "source_id", "source_identity", "name", "scope"); err != nil {
			return nil, err
		}
		if err := learningReviewObject(fields, "expected_head", &result.ExpectedHead, nil, "version", "state_version", "exists", "enabled", "managed"); err != nil {
			return nil, err
		}
		if !bytes.Equal(fields["overlay_base"], []byte("null")) {
			var base harness.SkillRef
			if err := learningReviewObject(fields, "overlay_base", &base, nil, "sourceId", "sourceIdentity", "name", "version", "hash"); err != nil {
				return nil, err
			}
			result.OverlayBase = &base
		}
		return result, nil
	}
	var result harness.SkillLearningPatternIgnoreRequest
	if decoder.Decode(&result) != nil {
		return nil, managementInvalid("invalid learning ignore")
	}
	return result, nil
}

// encoding/json accepts case-insensitive aliases. Management preconditions must
// instead have one exact wire spelling, including nested identities and bools.
func learningReviewObject(fields map[string]json.RawMessage, key string, dst any, optional []string, required ...string) error {
	var values map[string]json.RawMessage
	if json.Unmarshal(fields[key], &values) != nil || values == nil {
		return managementInvalid("invalid " + key)
	}
	for field, value := range values {
		allowed := false
		for _, candidate := range append(append([]string{}, required...), optional...) {
			allowed = allowed || field == candidate
		}
		if !allowed || bytes.Equal(value, []byte("null")) {
			return managementInvalid("invalid " + key + "." + field)
		}
	}
	return managementSkillObject(fields, key, dst, required...)
}

func learningCreationRetryRequest(raw json.RawMessage, enqueue bool) (any, error) {
	canonical, err := skillbehavior.CanonicalJSON(raw)
	var fields map[string]json.RawMessage
	if len(raw) > 8<<10 || err != nil || json.Unmarshal(canonical, &fields) != nil {
		return nil, managementInvalid("invalid learning creation retry")
	}
	keys := []string{"pattern_id", "expected_pattern_revision", "expected_state_version", "scope", "previous_generation_id"}
	if enqueue {
		keys = append(keys, "request_key", "expected_generation_state_version", "previous_proposal_id", "expected_proposal_state_version", "target", "expected_head", "overlay_base", "evidence_reservation_id", "expected_retry_number")
	}
	if len(fields) != len(keys) {
		return nil, managementInvalid("invalid creation retry fields")
	}
	for _, key := range keys {
		if len(fields[key]) == 0 || key != "overlay_base" && bytes.Equal(fields[key], []byte("null")) {
			return nil, managementInvalid("missing creation retry field")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if !enqueue {
		var result harness.SkillLearningNewSkillRetryPreparationRequest
		if decoder.Decode(&result) != nil {
			return nil, managementInvalid("invalid creation retry preparation")
		}
		return result, nil
	}
	var result harness.SkillLearningNewSkillRetryRequest
	if decoder.Decode(&result) != nil {
		return nil, managementInvalid("invalid creation retry")
	}
	if err := learningReviewObject(fields, "target", &result.Target, []string{"workspace"}, "source_id", "source_identity", "name", "scope"); err != nil {
		return nil, err
	}
	if err := learningReviewObject(fields, "expected_head", &result.ExpectedHead, nil, "version", "state_version", "exists", "enabled", "managed"); err != nil {
		return nil, err
	}
	if !bytes.Equal(fields["overlay_base"], []byte("null")) {
		return nil, managementInvalid("creation retry requires absent overlay")
	}
	return result, nil
}

func learningSourceRequest(raw json.RawMessage) (harness.SkillLearningSourceRequest, error) {
	var result harness.SkillLearningSourceRequest
	if len(raw) > 2048 {
		return result, managementInvalid("learning source exceeds bounds")
	}
	canonical, err := skillbehavior.CanonicalJSON(raw)
	var fields map[string]json.RawMessage
	keys := []string{"kind", "id", "expected_revision", "scope", "experience_id"}
	if err != nil || json.Unmarshal(canonical, &fields) != nil || len(fields) != len(keys) {
		return result, managementInvalid("invalid learning source")
	}
	for _, key := range keys {
		if len(fields[key]) == 0 || bytes.Equal(fields[key], []byte("null")) {
			return result, managementInvalid("missing learning source field")
		}
	}
	if json.Unmarshal(canonical, &result) != nil || result.ExpectedRevision < 1 {
		return result, managementInvalid("invalid learning source condition")
	}
	return result, nil
}

func (c *Client) managementLearning(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	if request.Operation == "skill.learning.status" {
		if err := proposalFields(request); err != nil {
			return nil, err
		}
		return c.skillLearningStatus(ctx)
	}
	allowed := []string{"session_id"}
	switch request.Operation {
	case "skill.learning.control.get", "skill.learning.episode.start":
	case "skill.learning.diagnostics":
		allowed = append(allowed, "limit")
	case "skill.learning.extraction.retry":
		allowed = append(allowed, "job_id", "expected_epoch", "expected_state_version")
	case "skill.learning.control.set":
		allowed = append(allowed, "control")
	case "skill.learning.pattern.ignore", "skill.learning.pattern.adjudicate":
		allowed = append(allowed, "review")
	case "skill.learning.creation_retry.prepare", "skill.learning.creation_retry.enqueue":
		allowed = append(allowed, "retry")
	case "skill.learning.source.get":
		allowed = append(allowed, "source")
	case "skill.learning.experiences", "skill.learning.jobs", "skill.learning.patterns", "skill.learning.matches", "skill.learning.workspace.patterns", "skill.learning.workspace.matches":
		allowed = append(allowed, "cursor", "limit")
	case "skill.learning.lineage.link":
		allowed = append(allowed, "source_session_id", "source_episode_id", "expected_epoch", "expected_source_epoch")
	case "skill.learning.cancel":
		allowed = append(allowed, "job_id", "expected_state_version")
	default:
		return nil, managementInvalid("unknown skill learning operation")
	}
	if err := proposalFields(request, allowed...); err != nil {
		return nil, err
	}
	id, err := managementID(request.Fields, "session_id")
	if err != nil {
		return nil, err
	}
	if _, err = c.service.Store.ManagementSession(ctx, id); err != nil {
		return nil, err
	}
	switch request.Operation {
	case "skill.learning.diagnostics":
		limit, err := optionalProposalInteger(request.Fields, "limit", 50)
		if err != nil || limit < 1 || limit > 100 {
			return nil, managementInvalid("limit must be 1..100")
		}
		return c.learning.Diagnostics(ctx, id, int(limit))
	case "skill.learning.extraction.retry":
		job, err := managementID(request.Fields, "job_id")
		if err != nil {
			return nil, err
		}
		epoch, err := optionalProposalInteger(request.Fields, "expected_epoch", 0)
		if err != nil || epoch < 1 {
			return nil, managementInvalid("expected_epoch is required")
		}
		version, err := optionalProposalInteger(request.Fields, "expected_state_version", 0)
		if err != nil || version < 1 {
			return nil, managementInvalid("expected_state_version is required")
		}
		retried, err := c.learning.RetryExtraction(ctx, id, harness.SkillLearningExtractionRetryRequest{JobID: job, ExpectedEpoch: epoch, ExpectedStateVersion: version})
		if err != nil {
			return nil, err
		}
		return map[string]any{"job": retried}, nil
	case "skill.learning.source.get":
		source, err := learningSourceRequest(request.Fields["source"])
		if err != nil {
			return nil, err
		}
		return c.learning.Source(ctx, id, source)
	case "skill.learning.control.get":
		return c.learning.Control(ctx, id)
	case "skill.learning.control.set":
		control, err := learningControlRequest(request.Fields["control"])
		if err != nil {
			return nil, err
		}
		return c.learning.SetControl(ctx, id, control)
	case "skill.learning.pattern.ignore", "skill.learning.pattern.adjudicate":
		adjudicate := request.Operation == "skill.learning.pattern.adjudicate"
		review, err := learningReviewRequest(request.Fields["review"], adjudicate)
		if err != nil {
			return nil, err
		}
		if adjudicate {
			return c.learning.AdjudicatePattern(ctx, id, review.(harness.SkillLearningPatternAdjudicationRequest))
		}
		return c.learning.SetPatternIgnored(ctx, id, review.(harness.SkillLearningPatternIgnoreRequest))
	case "skill.learning.episode.start":
		episode, err := c.learning.StartEpisode(ctx, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"episode_id": episode}, nil
	case "skill.learning.creation_retry.prepare", "skill.learning.creation_retry.enqueue":
		enqueue := request.Operation == "skill.learning.creation_retry.enqueue"
		retry, err := learningCreationRetryRequest(request.Fields["retry"], enqueue)
		if err != nil {
			return nil, err
		}
		if c.evolution == nil {
			return nil, harness.ErrPermissionDenied
		}
		if enqueue {
			return c.retrySkillLearningNewSkill(ctx, id, retry.(harness.SkillLearningNewSkillRetryRequest))
		}
		return c.evolution.PrepareNewSkillRetry(ctx, id, retry.(harness.SkillLearningNewSkillRetryPreparationRequest))
	case "skill.learning.lineage.link":
		source, err := managementID(request.Fields, "source_session_id")
		if err != nil {
			return nil, err
		}
		episode, err := managementID(request.Fields, "source_episode_id")
		if err != nil {
			return nil, err
		}
		if _, err = c.service.Store.ManagementSession(ctx, source); err != nil {
			return nil, err
		}
		epoch, err := optionalProposalInteger(request.Fields, "expected_epoch", 0)
		if err != nil || epoch < 1 {
			return nil, managementInvalid("expected_epoch is required")
		}
		sourceEpoch, err := optionalProposalInteger(request.Fields, "expected_source_epoch", 0)
		if err != nil || sourceEpoch < 1 {
			return nil, managementInvalid("expected_source_epoch is required")
		}
		linked, err := c.learning.LinkEpisodeLineage(ctx, id, harness.SkillLearningLineageRequest{SourceSessionID: source, SourceEpisodeID: episode, ExpectedEpoch: epoch, ExpectedSourceEpoch: sourceEpoch})
		if err != nil {
			return nil, err
		}
		return map[string]any{"episode_id": linked}, nil
	case "skill.learning.cancel":
		job, err := managementID(request.Fields, "job_id")
		if err != nil {
			return nil, err
		}
		version, err := optionalProposalInteger(request.Fields, "expected_state_version", 0)
		if err != nil || version < 1 {
			return nil, managementInvalid("expected_state_version is required")
		}
		if err = c.learning.CancelExtraction(ctx, id, job, version); err != nil {
			return nil, err
		}
		return map[string]any{"cancelled": true}, nil
	default:
		var cursor string
		if raw, exists := request.Fields["cursor"]; exists && json.Unmarshal(raw, &cursor) != nil {
			return nil, managementInvalid("invalid cursor")
		}
		limit, err := optionalProposalInteger(request.Fields, "limit", 50)
		if err != nil || limit < 1 || limit > 100 {
			return nil, managementInvalid("limit must be 1..100")
		}
		if request.Operation == "skill.learning.jobs" {
			return c.learning.Jobs(ctx, id, cursor, int(limit))
		}
		if request.Operation == "skill.learning.patterns" {
			return c.learning.Patterns(ctx, id, cursor, int(limit))
		}
		if request.Operation == "skill.learning.workspace.patterns" {
			return c.learning.WorkspacePatterns(ctx, id, cursor, int(limit))
		}
		if request.Operation == "skill.learning.workspace.matches" {
			return c.learning.WorkspaceMatches(ctx, id, cursor, int(limit))
		}
		if request.Operation == "skill.learning.matches" {
			return c.learning.Matches(ctx, id, cursor, int(limit))
		}
		return c.learning.Experiences(ctx, id, cursor, int(limit))
	}
}
