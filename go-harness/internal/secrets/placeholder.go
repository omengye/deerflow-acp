package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Placeholder grammar: {{secret:NAME}} in a string of tool arguments stands
// for the current value of secret NAME, and {{secret:NAME:ENC}} for the value
// in a marker encoding (json, url, urlpath, trim, b64, b64raw, b64url,
// b64rawurl). The host substitutes values only into the arguments it hands to
// a tool, so conversations, approval prompts and receipts keep the names.
const placeholderOpen = "{{secret:"

var placeholderPattern = regexp.MustCompile(`^\{\{secret:([A-Z_][A-Z0-9_]*)(?::([a-z0-9]{1,12}))?\}\}`)

// MaxSubstitutedBytes bounds tool arguments after substitution.
const MaxSubstitutedBytes = 4 << 20

type placeholder struct {
	start, end int
	name       string
	suffix     string // "" or ":ENC"
}

// parsePlaceholders returns the placeholders of text in order. Text that
// opens like a placeholder but is not one is an error, so that a misspelled
// placeholder never reaches a tool as literal text.
func parsePlaceholders(text string) ([]placeholder, error) {
	var out []placeholder
	for offset := 0; ; {
		i := strings.Index(text[offset:], placeholderOpen)
		if i < 0 {
			return out, nil
		}
		start := offset + i
		m := placeholderPattern.FindStringSubmatch(text[start:])
		if m == nil || !ValidName(m[1]) {
			return nil, fmt.Errorf("%w: malformed secret placeholder; write {{secret:NAME}} with the secret's name in uppercase letters, digits and underscores, optionally with an encoding such as {{secret:NAME:url}}", ErrInvalid)
		}
		suffix := ""
		if m[2] != "" {
			suffix = ":" + m[2]
			if !slices.Contains(restoreSuffixes, suffix) {
				return nil, fmt.Errorf("%w: secret placeholder for %s has unknown encoding %q; use json, url, urlpath, trim, b64, b64raw, b64url or b64rawurl", ErrInvalid, m[1], m[2])
			}
		}
		out = append(out, placeholder{start: start, end: start + len(m[0]), name: m[1], suffix: suffix})
		offset = start + len(m[0])
	}
}

// rewriteJSONStrings calls rewrite with the decoded text of every string
// literal of the JSON document s that contains a placeholder opening, and
// re-encodes the literals that rewrite changes. Other bytes stay as they are.
// Working at the decoded level keeps the document valid whatever a value
// contains, and finds placeholders written with JSON escapes.
func rewriteJSONStrings(s string, rewrite func(string) (string, error)) (string, error) {
	return rewriteMarkedStrings(s, placeholderOpen, false, rewrite)
}

func rewriteMarkedStrings(s, prefix string, rejectKeys bool, rewrite func(string) (string, error)) (string, error) {
	if !json.Valid([]byte(s)) {
		return "", fmt.Errorf("%w: tool arguments are not valid JSON", ErrInvalid)
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		end, escaped := i+1, false
		for ; s[end] != '"'; end++ {
			if s[end] == '\\' {
				escaped = true
				end++
			}
		}
		text := s[i+1 : end]
		if escaped {
			if err := json.Unmarshal([]byte(s[i:end+1]), &text); err != nil {
				return "", err
			}
		}
		if strings.Contains(text, prefix) {
			if rejectKeys && strings.HasPrefix(strings.TrimLeft(s[end+1:], " \t\r\n"), ":") {
				return "", errors.New("credential markers are permitted only in argument values")
			}
			replaced, err := rewrite(text)
			if err != nil {
				return "", err
			}
			if replaced != text {
				b.WriteString(s[last:i])
				b.WriteByte('"')
				b.WriteString(jsonEscaped(replaced, false))
				b.WriteByte('"')
				last = end + 1
			}
		}
		i = end
	}
	if last == 0 {
		return s, nil
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// PlaceholderNames returns the sorted, distinct names that placeholders in
// the string literals of the JSON document arguments refer to.
func PlaceholderNames(arguments string) ([]string, error) {
	seen := map[string]bool{}
	_, err := rewriteJSONStrings(arguments, func(text string) (string, error) {
		found, err := parsePlaceholders(text)
		for _, p := range found {
			seen[p.name] = true
		}
		return text, err
	})
	if err != nil || len(seen) == 0 {
		return nil, err
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// SubstitutePlaceholders replaces the placeholders in the string literals of
// the JSON document arguments with the values of resolved secrets. The result
// holds plaintext: hand it to the tool only and never record it.
func SubstitutePlaceholders(arguments string, values map[string]Secret) (string, error) {
	size := len(arguments)
	out, err := rewriteJSONStrings(arguments, func(text string) (string, error) {
		found, err := parsePlaceholders(text)
		if err != nil || len(found) == 0 {
			return text, err
		}
		var b strings.Builder
		last := 0
		for _, p := range found {
			secret, ok := values[p.name]
			if !ok {
				return "", &MissingError{Names: []string{p.name}}
			}
			isBase64 := p.suffix == ":b64" || p.suffix == ":b64raw" || p.suffix == ":b64url" || p.suffix == ":b64rawurl"
			if secret.Kind == KindFile && !isBase64 {
				return "", fmt.Errorf("%w: file secret %s cannot be substituted as plain text; pass it as {{secret:%s:b64}} or inject it as a file via execute/skill_run", ErrInvalid, p.name, p.name)
			}
			value, ok := restoreText(p.suffix, secret.Value)
			if !ok || !utf8.ValidString(value) || (!isBase64 && strings.ContainsRune(value, 0)) {
				return "", fmt.Errorf("%w: secret %s is binary; pass it as {{secret:%s:b64}}", ErrInvalid, p.name, p.name)
			}
			if size += len(value) - (p.end - p.start); size > MaxSubstitutedBytes {
				return "", fmt.Errorf("%w: tool arguments exceed %d bytes once secrets are substituted", ErrInvalid, MaxSubstitutedBytes)
			}
			b.WriteString(text[last:p.start])
			b.WriteString(value)
			last = p.end
		}
		b.WriteString(text[last:])
		return b.String(), nil
	})
	if err != nil {
		return "", err
	}
	if len(out) > MaxSubstitutedBytes {
		return "", fmt.Errorf("%w: tool arguments exceed %d bytes once secrets are substituted", ErrInvalid, MaxSubstitutedBytes)
	}
	return out, nil
}
