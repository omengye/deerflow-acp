package memory

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
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

// CheckExternalContextWrite applies value-level provenance to an explicit
// write made after the session consumed external content. Injected content
// tries to persist actionable values (links, domains, paths, commands, contact
// addresses, markup), so every such token in the fact must have been typed by
// the user in the current message. Ordinary text, including "A/B" or "1/2"
// the user wrote, is unaffected; a value that exists only in third-party
// content is rejected. Multi-line facts and secret-like values are rejected.
func CheckExternalContextWrite(content, evidence, userText string) error {
	if utf8.RuneCountInString(strings.TrimSpace(evidence)) < 4 {
		return fmt.Errorf("%w: this session read external content; quote a longer phrase from the current user message as evidence", harness.ErrInvalidInput)
	}
	if strings.ContainsAny(content, "\r\n") || credentialLike.MatchString(strings.ToLower(content)) {
		return fmt.Errorf("%w: this session read external content; memory written now must be a single line without secret-like values", harness.ErrInvalidInput)
	}
	if ungrounded := UngroundedActionableTokens(content, userText); len(ungrounded) > 0 {
		return fmt.Errorf("%w: this session read external content; links, domains, paths, addresses or markup in memory must be copied from the current user message, not from tool results: %s", harness.ErrInvalidInput, strings.Join(ungrounded, " "))
	}
	return nil
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
