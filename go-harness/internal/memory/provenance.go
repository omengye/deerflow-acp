package memory

import (
	"context"
	"database/sql"
	"regexp"
	"strings"

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
	UnknownProvenance         = "unknown"
	UserSupportedProvenance   = "user_supported"
)

// Provenance is independent of validation policy. Historical facts without
// verifiable context retain "unknown"; a policy upgrade never establishes trust.
func Provenance(f Fact) string {
	if f.Source.Provenance != "" {
		return f.Source.Provenance
	}
	if f.Source.PolicyVersion == ExternalContextPolicy {
		return ExternalContextProvenance
	}
	return UnknownProvenance
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
		if harness.UntrustedMemoryContentTool(name.String) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// CheckExternalContextWrite is a compatibility entry point for the same
// deterministic declaration validator used by all model write channels.
func CheckExternalContextWrite(content, evidence, userText string) error {
	return validateLegacyContent(content, evidence, userText)
}

// actionableRun is a maximal run of characters that can form a link, domain,
// file path, command fragment, address or markup. CJK text and spaces split it.
var actionableRun = regexp.MustCompile(`[A-Za-z0-9._~:/\\@%+=?&#<>$-]+`)

// domainLike matches a host or file name with an alphabetic final label, such
// as evil.example or setup.sh, but not version or decimal numbers (v1.2, 3.14).
var domainLike = regexp.MustCompile(`(?i)^[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z][a-z0-9-]*$`)

// UngroundedActionableTokens returns the actionable tokens of content that do
// not appear in userText (case-insensitive). A token is actionable when it is
// a URL, domain or file name, path, address or markup, or contains shell
// metacharacters; plain words and numbers are never actionable.
func UngroundedActionableTokens(content, userText string) []string {
	user := strings.ToLower(userText)
	var out []string
	for _, token := range actionableRun.FindAllString(content, -1) {
		token = strings.TrimRight(token, ".:?#&=-")
		if !actionableToken(token) || strings.Contains(user, strings.ToLower(token)) {
			continue
		}
		out = append(out, token)
	}
	return out
}

func actionableToken(token string) bool {
	if token == "" {
		return false
	}
	if strings.ContainsAny(token, `\@<>$`) || strings.Contains(token, "://") {
		return true
	}
	if strings.Contains(token, "/") {
		// A short ratio or alternative such as A/B, 1/2 or TCP/IP stays plain
		// text; anything path-like (leading or repeated separators, dotted
		// segments) is actionable.
		if strings.HasPrefix(token, "/") || strings.HasPrefix(token, "~") || strings.HasPrefix(token, ".") || strings.Count(token, "/") > 1 {
			return true
		}
		for _, part := range strings.Split(token, "/") {
			if part == "" || domainLike.MatchString(part) {
				return true
			}
		}
		return false
	}
	return domainLike.MatchString(token)
}
