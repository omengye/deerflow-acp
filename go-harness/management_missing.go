package deerflow

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

// The authenticated management channel is the operator authority. Root-session
// and run membership are checked together by the service in its transaction;
// an ACP attachment or a connection-supplied owner is not required here.
func (c *Client) managementMissing(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	switch request.Operation {
	case "skill.missing.submit":
		if err := proposalFields(request, "session_id", "run_id", "expected_report_version", "request_key", "kind", "summary"); err != nil {
			return nil, err
		}
		var report harness.SkillMissingReportRequest
		for _, field := range []struct {
			key string
			out any
		}{
			{"session_id", &report.SessionID},
			{"run_id", &report.RunID},
			{"expected_report_version", &report.ExpectedReportVersion},
			{"request_key", &report.RequestKey},
			{"kind", &report.Kind},
			{"summary", &report.Summary},
		} {
			if err := managementFeedbackField(request.Fields, field.key, field.out); err != nil {
				return nil, err
			}
		}
		return c.evolution.SubmitMissingReport(ctx, report)
	case "skill.missing.get", "skill.missing.list", "skill.missing.history", "skill.missing.targets":
		allowed := []string{"session_id"}
		requiresRun := request.Operation == "skill.missing.get" || request.Operation == "skill.missing.history"
		if requiresRun {
			allowed = append(allowed, "run_id")
		}
		if request.Operation != "skill.missing.get" {
			allowed = append(allowed, "cursor", "limit")
		}
		if err := proposalFields(request, allowed...); err != nil {
			return nil, err
		}
		var sessionID, runID string
		if err := managementFeedbackField(request.Fields, "session_id", &sessionID); err != nil {
			return nil, err
		}
		if requiresRun {
			if err := managementFeedbackField(request.Fields, "run_id", &runID); err != nil {
				return nil, err
			}
		}
		if request.Operation == "skill.missing.get" {
			return c.evolution.MissingReport(ctx, sessionID, runID)
		}
		cursor, limit, err := managementMissingPage(request.Fields)
		if err != nil {
			return nil, err
		}
		if request.Operation == "skill.missing.list" {
			return c.evolution.QueryMissingReports(ctx, sessionID, cursor, limit)
		}
		if request.Operation == "skill.missing.targets" {
			return c.evolution.QueryMissingReportTargets(ctx, sessionID, cursor, limit)
		}
		return c.evolution.QueryMissingReportHistory(ctx, sessionID, runID, cursor, limit)
	}
	return nil, managementInvalid("unsupported missing skill report operation")
}

func managementMissingPage(fields map[string]json.RawMessage) (string, int, error) {
	var cursor string
	if _, present := fields["cursor"]; present {
		if err := managementFeedbackField(fields, "cursor", &cursor); err != nil {
			return "", 0, err
		}
		if len(cursor) > 2048 || !utf8.ValidString(cursor) || strings.ContainsRune(cursor, 0) {
			return "", 0, managementInvalid("invalid cursor")
		}
	}
	limit, err := optionalProposalInteger(fields, "limit", 50)
	if err != nil {
		return "", 0, err
	}
	if limit > 100 {
		return "", 0, managementInvalid("limit must be 1..100")
	}
	return cursor, int(limit), nil
}
