package skills

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var editSecretAssignment = regexp.MustCompile(`(?im)(["']?(?:token|api[_-]?key|access[_-]?token|secret|password|private[_-]?key|authorization)["']?\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)
var editBearer = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9_./+=-]+`)

func sensitiveEditKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "authorization") || strings.Contains(key, "api_key") || strings.Contains(key, "apikey") || strings.Contains(key, "private_key")
}

func redactEditText(text string) string {
	text = editSecretAssignment.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = editBearer.ReplaceAllString(text, "Bearer [REDACTED]")
	if strings.Contains(text, "-----BEGIN") && strings.Contains(text, "PRIVATE KEY") {
		return "[REDACTED private key material]"
	}
	return text
}

func editSecrets(value any, out *[]string) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if sensitiveEditKey(key) {
				if text, ok := item.(string); ok && text != "" {
					*out = append(*out, text)
				}
			}
			editSecrets(item, out)
		}
	case []any:
		for _, item := range v {
			editSecrets(item, out)
		}
	}
}

func parseEditJSON(data []byte) (any, error) {
	// Reject duplicate keys rather than silently dropping credential fields
	// when json_set serializes the document again.
	check := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueEditJSON(check); err != nil {
		return nil, invalid("JSON contains duplicate keys or invalid values")
	}
	if _, err := check.Token(); err != io.EOF {
		return nil, invalid("JSON must contain exactly one value")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, invalid("json_set requires a valid JSON file and value")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, invalid("JSON must contain exactly one value")
	}
	return v, nil
}

func uniqueEditJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return invalid("duplicate JSON key")
			}
			seen[s] = true
		}
		if err := uniqueEditJSON(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func literalEditSecrets(data []byte) []string {
	var secrets []string
	for _, match := range editSecretAssignment.FindAllStringSubmatch(string(data), -1) {
		v := match[2]
		// Script expressions such as token = os.environ.get(...) are not
		// credentials. Quoted literals are conservatively kept out of diffs.
		if len(v) > 2 && (v[0] == '\'' || v[0] == '"') {
			secrets = append(secrets, v[1:len(v)-1])
		}
	}
	return secrets
}

func setEditJSON(data []byte, pointer string, value json.RawMessage) ([]byte, error) {
	if !strings.HasPrefix(pointer, "/") || len(pointer) > 512 || len(value) == 0 {
		return nil, invalid("json_set requires a non-root JSON pointer and value")
	}
	root, err := parseEditJSON(data)
	if err != nil {
		return nil, err
	}
	replacement, err := parseEditJSON(value)
	if err != nil {
		return nil, err
	}
	var secrets []string
	editSecrets(replacement, &secrets)
	if len(secrets) != 0 || redactEditText(string(value)) != string(value) {
		return nil, denied("edit credentials through host settings, not skill edits")
	}
	parts := strings.Split(pointer[1:], "/")
	current := root
	for i, raw := range parts {
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(raw, "~1", ""), "~0", ""), "~") {
			return nil, invalid("invalid JSON pointer escape")
		}
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		if sensitiveEditKey(key) {
			return nil, denied("credential fields cannot be edited by the skill tool")
		}
		var old any
		var assign func(any)
		switch v := current.(type) {
		case map[string]any:
			var exists bool
			old, exists = v[key]
			if !exists {
				return nil, invalid("JSON pointer must select an existing field")
			}
			assign = func(x any) { v[key] = x }
		case []any:
			n, e := strconv.Atoi(key)
			if e != nil || n < 0 || n >= len(v) || strconv.Itoa(n) != key {
				return nil, invalid("invalid JSON array index")
			}
			old, assign = v[n], func(x any) { v[n] = x }
		default:
			return nil, invalid("JSON pointer does not resolve")
		}
		if i == len(parts)-1 {
			var nested []string
			editSecrets(old, &nested)
			if len(nested) != 0 {
				return nil, denied("cannot replace an object containing credentials")
			}
			assign(replacement)
		} else {
			current = old
		}
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	if bytes.Contains(data, []byte("\r\n")) {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out, nil
}

func editPreview(before, after []byte, secrets []string) string {
	sanitize := func(data []byte) []string {
		s := string(data)
		for _, secret := range secrets {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
		return strings.Split(strings.ReplaceAll(redactEditText(s), "\r\n", "\n"), "\n")
	}
	a, b := sanitize(before), sanitize(after)
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	var out strings.Builder
	fmt.Fprintf(&out, "@@ line %d @@\n", start+1)
	for _, line := range a[start:endA] {
		out.WriteString("-" + line + "\n")
	}
	for _, line := range b[start:endB] {
		out.WriteString("+" + line + "\n")
	}
	return out.String()
}

func applyFileEdits(base map[string][]byte, edits []harness.SkillFileEdit) (map[string][]byte, []harness.SkillEditChange, error) {
	if len(edits) < 1 || len(edits) > 16 {
		return nil, nil, invalid("an edit requires 1..16 file changes")
	}
	files := make(map[string][]byte, len(base))
	var secrets []string
	for name, data := range base {
		files[name] = bytes.Clone(data)
		if v, err := parseEditJSON(data); err == nil {
			editSecrets(v, &secrets)
		}
		secrets = append(secrets, literalEditSecrets(data)...)
	}
	request, _ := json.Marshal(edits)
	for _, secret := range secrets {
		encoded, _ := json.Marshal(secret)
		if bytes.Contains(request, encoded[1:len(encoded)-1]) {
			return nil, nil, denied("do not supply credential values in edit arguments; use json_set on non-secret fields")
		}
	}
	for _, edit := range edits {
		before, exists := files[edit.Path]
		if !validRelative(edit.Path) || !exists || len(before) > 128<<10 || !utf8.Valid(before) || bytes.IndexByte(before, 0) >= 0 {
			return nil, nil, invalid("edit must select an existing bounded UTF-8 skill file")
		}
		if edit.Operation != "json_set" && strings.EqualFold(path.Ext(edit.Path), ".json") && len(literalEditSecrets(before)) > 0 {
			return nil, nil, denied("use json_set to preserve credentials without exposing them")
		}
		var after []byte
		switch edit.Operation {
		case "replace":
			if edit.Find == nil || edit.Replace == nil || *edit.Find == "" || edit.Content != nil || edit.Pointer != "" || len(edit.Value) != 0 {
				return nil, nil, invalid("replace requires only find and replace")
			}
			find, replacement := *edit.Find, *edit.Replace
			if bytes.Contains(before, []byte("\r\n")) {
				find = strings.ReplaceAll(strings.ReplaceAll(find, "\r\n", "\n"), "\n", "\r\n")
				replacement = strings.ReplaceAll(strings.ReplaceAll(replacement, "\r\n", "\n"), "\n", "\r\n")
			}
			if strings.Count(string(before), find) != 1 {
				return nil, nil, invalid("replace must match exactly once in the installed file")
			}
			after = []byte(strings.Replace(string(before), find, replacement, 1))
		case "write":
			if edit.Content == nil || edit.Find != nil || edit.Replace != nil || edit.Pointer != "" || len(edit.Value) != 0 {
				return nil, nil, invalid("write requires only content")
			}
			after = []byte(*edit.Content)
		case "json_set":
			if edit.Content != nil || edit.Find != nil || edit.Replace != nil {
				return nil, nil, invalid("json_set requires only pointer and value")
			}
			var err error
			after, err = setEditJSON(before, edit.Pointer, edit.Value)
			if err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, invalid("unsupported file edit operation")
		}
		if len(after) > 128<<10 || !utf8.Valid(after) || bytes.IndexByte(after, 0) >= 0 {
			return nil, nil, invalid("edited files must be bounded UTF-8 text")
		}
		if edit.Operation != "json_set" {
			oldSecrets, newSecrets := editSecretAssignment.FindAllString(string(before), -1), editSecretAssignment.FindAllString(string(after), -1)
			if !slices.Equal(oldSecrets, newSecrets) {
				return nil, nil, denied("credential-like assignments require host settings")
			}
			if !slices.Equal(editBearer.FindAllString(string(before), -1), editBearer.FindAllString(string(after), -1)) {
				return nil, nil, denied("bearer credentials require host settings")
			}
		}
		files[edit.Path] = after
	}
	var changes []harness.SkillEditChange
	previewBytes := 0
	for name, after := range files {
		before := base[name]
		if bytes.Equal(before, after) {
			continue
		}
		for _, secret := range secrets {
			if bytes.Count(before, []byte(secret)) != bytes.Count(after, []byte(secret)) {
				return nil, nil, denied("skill edits cannot change or copy existing credentials")
			}
		}
		preview := editPreview(before, after, secrets)
		previewBytes += len(preview)
		if previewBytes > 48<<10 {
			return nil, nil, invalid("edit diff exceeds the approval limit; make a smaller change")
		}
		script := !strings.EqualFold(path.Ext(name), ".md") && !strings.EqualFold(path.Ext(name), ".json") || bytes.HasPrefix(after, []byte("#!"))
		changes = append(changes, harness.SkillEditChange{Path: name, BeforeSHA256: digest(before), AfterSHA256: digest(after), Preview: preview, Script: script})
	}
	if len(changes) == 0 {
		return nil, nil, invalid("edit has no changes")
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return files, changes, nil
}
