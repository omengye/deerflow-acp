package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Explicit memory writes are user-requested and quote the current user
// message. When the session has already consumed third-party content (web,
// browser, MCP, attachments), injected text in that content may have steered
// the model's wording, so the fact keeps that provenance permanently and is
// labelled whenever it is shown to a model again.
const (
	ExplicitPolicy        = "personal-explicit-v1"
	ExternalContextPolicy = "personal-explicit-external-context-v1"
	// ExternalContextProvenance labels such facts in model-visible memory.
	ExternalContextProvenance = "external_context"
)

// Provenance returns the model-visible provenance label of a fact, or "" for
// facts that carry no external-content provenance.
func Provenance(f Fact) string {
	if f.Source.PolicyVersion == ExternalContextPolicy {
		return ExternalContextProvenance
	}
	return ""
}

// SessionConsumedExternalContent reports whether any tool call in the session
// returned (or may have returned) output from an external-content tool.
// Uncertain and failed receipts count: their partial output may already be in
// the model context.
func (s *Store) SessionConsumedExternalContent(ctx context.Context, sessionID string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT COALESCE(json_extract(CAST(receipt AS TEXT),'$.toolName'),'') FROM harness_tool_receipts WHERE session_id=?`, sessionID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name sql.NullString
		if err = rows.Scan(&name); err != nil {
			return false, err
		}
		if harness.ExternalContentTool(name.String) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// CheckExternalContextWrite applies the automatic-extraction content rules to
// an explicit write made after the session consumed external content: the
// quote must be substantive and the fact cannot carry links, paths, markup or
// secret-like values that injected content typically tries to persist.
func CheckExternalContextWrite(content, evidence string) error {
	if utf8.RuneCountInString(strings.TrimSpace(evidence)) < 4 {
		return fmt.Errorf("%w: this session read external content; quote a longer phrase from the current user message as evidence", harness.ErrInvalidInput)
	}
	lower := strings.ToLower(content)
	if strings.ContainsAny(content, "\r\n<>") || strings.Contains(lower, "://") || strings.ContainsAny(content, `/\`) || credentialLike.MatchString(lower) {
		return fmt.Errorf("%w: this session read external content; memory written now cannot contain links, paths, markup or secret-like values", harness.ErrInvalidInput)
	}
	return nil
}
