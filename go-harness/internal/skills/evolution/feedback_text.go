package evolution

import (
	"regexp"
	"strings"
	"unicode"
)

// Feedback is explicitly authored, bounded text, never a copied transcript.
// These conservative patterns remove common credential forms; they are not a
// guarantee that arbitrary prose cannot contain a secret. The raw text is never
// persisted or returned in errors, including through the idempotency record.
var feedbackRedactions = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)https?://[^\s<>"']+`),
	regexp.MustCompile(`(?i)\b(?:proxy[-_]?authorization|authorization|set[-_]?cookie|cookie)["']?[ \t]*[:=][ \t]*[^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*`),
	regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)["']?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|private[_-]?key|password|passwd|secret|authorization|cookie|token)["']?[ \t]*[:=][ \t]*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`),
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,}|AKIA[A-Z0-9]{16})\b`),
}

func cleanFeedbackSummary(text string) (string, bool, error) {
	if !bounded(text, 2000) {
		return "", false, invalid("feedback summary exceeds text bounds")
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' || unicode.Is(unicode.Cf, r) {
			return "", false, invalid("feedback summary contains unsupported control characters")
		}
	}
	cleaned := strings.TrimSpace(text)
	redacted := false
	for _, pattern := range feedbackRedactions {
		if pattern.MatchString(cleaned) {
			cleaned = pattern.ReplaceAllString(cleaned, "[redacted]")
			redacted = true
		}
	}
	if len(cleaned) > 2000 {
		return "", false, invalid("feedback summary exceeds text bounds after redaction")
	}
	return cleaned, redacted, nil
}

func feedbackRequestKey(key string) bool {
	if len(key) < 1 || len(key) > 256 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func feedbackKind(kind string) bool {
	switch kind {
	case "helpful", "correction", "quality_failure", "network", "quota", "permission", "cancelled", "other", "withdrawn":
		return true
	}
	return false
}
