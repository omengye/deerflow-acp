package learning

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var instructionCredentialAssignment = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(?:["'])?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|private[_-]?key|password|passwd|secret|proxy[-_]?authorization|authorization|set[-_]?cookie|cookie|token)(?:["'])?[ \t]*[:=][ \t]*("[^"\r\n]*"|'[^'\r\n]*'|Bearer[ \t]+[^\s,;]+|[^\s,;]+)`)
var instructionLegacyCredentialAssignment = regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|private[_-]?key|password)[ \t]*[:=][ \t]*["']([^"'\r\n]{8,})["']`)
var instructionBearer = regexp.MustCompile(`(?i)\bBearer[ \t]+([^\s,;]+)`)
var instructionPrivateKey = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
var instructionCredentialPrefix = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,}|AKIA[A-Z0-9]{16})\b`)
var instructionURL = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+`)
var instructionEscape = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}(?:\\u[0-9a-fA-F]{4})?|["\\/bfnrt])`)
var instructionCredentialPlaceholder = regexp.MustCompile(`(?i)^(?:\[redacted\]|<(?:api[_-]?key|access[_-]?token|refresh[_-]?token|private[_-]?key|password|secret|token)>|(?:YOUR|EXAMPLE|SAMPLE)_(?:API_KEY|ACCESS_TOKEN|REFRESH_TOKEN|PRIVATE_KEY|PASSWORD|SECRET|TOKEN)|REPLACE_ME|CHANGE_ME|placeholder|\$\{(?:env:)?[A-Z_][A-Z0-9_]{0,127}\}|\$(?:env:)?[A-Z_][A-Z0-9_]{0,127}|%[A-Z_][A-Z0-9_]{0,127}%)$`)

// ValidatePromotionInstructions rejects known concrete credentials, not generic
// URLs or machine paths. It never rewrites the bytes bound to a version/review.
func ValidatePromotionInstructions(text string) error {
	if len(text) == 0 || len(text) > 128<<10 || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return invalid("generated instructions exceed text bounds")
	}
	text = normalizeLearningText(text)
	for range 16 {
		if instructionsContainCredential(text) {
			return invalid("generated instructions contain a credential-like literal")
		}
		// Markdown can contain embedded or repeatedly encoded JSON examples;
		// inspect decoded escapes without reserializing the candidate package.
		decoded := instructionEscape.ReplaceAllStringFunc(text, func(escape string) string {
			var value string
			if json.Unmarshal([]byte(`"`+escape+`"`), &value) != nil {
				return escape
			}
			return value
		})
		decoded = normalizeLearningText(decoded)
		if decoded == text {
			return nil
		}
		text = decoded
	}
	return invalid("generated instruction escapes exceed inspection bounds")
}

func instructionsContainCredential(text string) bool {
	if instructionPrivateKey.MatchString(text) || instructionCredentialPrefix.MatchString(text) {
		return true
	}
	// Preserve the legacy scanner's boundaryless assignments; only complete
	// placeholders, not a prefixed sensitive field, get the learning exception.
	for _, match := range instructionLegacyCredentialAssignment.FindAllStringSubmatch(text, -1) {
		if !instructionCredentialPlaceholder.MatchString(match[1]) {
			return true
		}
	}
	for _, match := range instructionCredentialAssignment.FindAllStringSubmatch(text, -1) {
		value := strings.Trim(match[1], "`\"'")
		if len(value) > 6 && strings.EqualFold(value[:6], "Bearer") && (value[6] == ' ' || value[6] == '\t') {
			value = strings.TrimSpace(value[6:])
		}
		value = instructionCredentialValue(value)
		if value != "" && !instructionCredentialPlaceholder.MatchString(value) {
			return true
		}
	}
	for _, match := range instructionBearer.FindAllStringSubmatch(text, -1) {
		value := instructionCredentialValue(match[1])
		switch strings.ToLower(value) {
		case "authentication", "authorization", "token", "tokens", "scheme":
			continue
		}
		if value != "" && !instructionCredentialPlaceholder.MatchString(value) {
			return true
		}
	}
	for _, match := range instructionURL.FindAllString(text, -1) {
		u, err := url.Parse(match)
		if err != nil {
			continue
		}
		if u.User != nil {
			if password, ok := u.User.Password(); ok && password != "" && !instructionCredentialPlaceholder.MatchString(password) {
				return true
			}
		}
		for key, values := range u.Query() {
			if !sensitiveJSONKey.MatchString(normalizeLearningText(key)) {
				continue
			}
			for _, value := range values {
				if value != "" && !instructionCredentialPlaceholder.MatchString(normalizeLearningText(value)) {
					return true
				}
			}
		}
	}
	return false
}

func instructionCredentialValue(value string) string {
	value = strings.Trim(value, "`\"'")
	if instructionCredentialPlaceholder.MatchString(value) {
		return value
	}
	return strings.TrimRight(value, "`\"'}]),.")
}
