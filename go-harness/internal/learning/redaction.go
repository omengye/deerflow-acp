package learning

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var sensitiveJSONKey = regexp.MustCompile(`(?i)^(?:api[_-]?key|access[_-]?token|refresh[_-]?token|private[_-]?key|password|passwd|secret|proxy[-_]?authorization|authorization|set[-_]?cookie|cookie|token)$`)

var sensitiveText = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(?:proxy[-_]?authorization|authorization|set[-_]?cookie|cookie)["']?[ \t]*[:=][ \t]*[^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*`),
	regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(?:\\*["'])?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|private[_-]?key|password|passwd|secret|authorization|cookie|token)(?:\\*["'])?[ \t]*[:=][ \t]*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`),
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,}|AKIA[A-Z0-9]{16})\b`),
	regexp.MustCompile(`(?i)https?://[^\s<>"']+`),
	regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`),
	regexp.MustCompile(`\b1[3-9][0-9]{9}\b`),
	regexp.MustCompile(`(?i)\b[A-Z]:[\\/][^\s"'<>\r\n]*`),
	regexp.MustCompile(`\\\\[^\s"'<>\r\n]+`),
	regexp.MustCompile(`(?:/Users|/home|/tmp|/private/var)/[^\s"'<>\r\n]*`),
}

func redact(text string, limit int) string {
	if !utf8.ValidString(text) {
		return ""
	}
	values := 4096
	text, _ = redactStructured(text, 0, &values)
	if len(text) > limit {
		for limit > 0 && !utf8.RuneStart(text[limit]) {
			limit--
		}
		text = text[:limit]
	}
	return text
}

func redactPlain(text string) string {
	// Normalize before matching: a hidden character inside token, Bearer or
	// a key delimiter must not make a secret survive the filtering pass.
	text = normalizeLearningText(text)
	for _, pattern := range sensitiveText {
		text = pattern.ReplaceAllString(text, "[redacted]")
	}
	return text
}

func normalizeLearningText(text string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return -1
		}
		return r
	}, text))
}

// Configured tools wrap file text in JSON strings. Decode those strings before
// applying text rules so escaped keys and values cannot bypass redaction.
func redactStructured(text string, depth int, remaining *int) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.ContainsRune(`{["`, rune(trimmed[0])) {
		return redactPlain(text), true
	}
	if depth >= 16 || len(trimmed) > 2<<20 || *remaining <= 0 {
		return "[redacted]", false
	}
	if !json.Valid([]byte(trimmed)) {
		return redactPlain(text), true
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	parseValues := 4096
	value, ok := readRedactionJSON(decoder, depth, &parseValues)
	if _, err := decoder.Token(); !ok || err != io.EOF {
		return "[redacted]", false
	}
	value, changed, ok := redactJSONValue(value, depth+1, remaining)
	if !ok {
		return "[redacted]", false
	}
	if !changed {
		return trimmed, true
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "[redacted]", false
	}
	return string(data), true
}

// Bound parsing before allocating a tree, and reject duplicate decoded keys;
// otherwise an overwritten sensitive value could survive in unchanged text.
func readRedactionJSON(decoder *json.Decoder, depth int, remaining *int) (any, bool) {
	if depth >= 16 || *remaining <= 0 {
		return nil, false
	}
	*remaining -= 1
	value, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if opening, container := value.(json.Delim); container {
		switch opening {
		case '{':
			object := map[string]any{}
			for decoder.More() {
				token, err := decoder.Token()
				key, valid := token.(string)
				if err != nil || !valid {
					return nil, false
				}
				if _, duplicate := object[key]; duplicate {
					return nil, false
				}
				entry, ok := readRedactionJSON(decoder, depth+1, remaining)
				if !ok {
					return nil, false
				}
				object[key] = entry
			}
			closing, err := decoder.Token()
			return object, err == nil && closing == json.Delim('}')
		case '[':
			array := []any{}
			for decoder.More() {
				entry, ok := readRedactionJSON(decoder, depth+1, remaining)
				if !ok {
					return nil, false
				}
				array = append(array, entry)
			}
			closing, err := decoder.Token()
			return array, err == nil && closing == json.Delim(']')
		default:
			return nil, false
		}
	}
	return value, true
}

func redactJSONValue(value any, depth int, remaining *int) (any, bool, bool) {
	if depth >= 16 || *remaining <= 0 {
		return nil, false, false
	}
	*remaining -= 1
	changed := false
	switch v := value.(type) {
	case string:
		text, ok := redactStructured(v, depth, remaining)
		return text, text != v, ok
	case map[string]any:
		cleanObject := make(map[string]any, len(v))
		for key, entry := range v {
			cleanKey := redactPlain(key)
			if _, duplicate := cleanObject[cleanKey]; duplicate {
				return nil, false, false
			}
			changed = changed || cleanKey != key
			if sensitiveJSONKey.MatchString(cleanKey) {
				if *remaining <= 0 {
					return nil, false, false
				}
				*remaining -= 1
				cleanObject[cleanKey], changed = "[redacted]", true
				continue
			}
			clean, different, ok := redactJSONValue(entry, depth+1, remaining)
			if !ok {
				return nil, false, false
			}
			cleanObject[cleanKey], changed = clean, changed || different
		}
		value = cleanObject
	case []any:
		for index, entry := range v {
			clean, different, ok := redactJSONValue(entry, depth+1, remaining)
			if !ok {
				return nil, false, false
			}
			v[index], changed = clean, changed || different
		}
	}
	return value, changed, true
}
